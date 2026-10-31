package main

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The services the plugin talks to; only tests change them. scripts/allowed-hosts.txt names each.
var (
	productionURL = "https://api.namecheap.com/xml.response"
	sandboxURL    = "https://api.sandbox.namecheap.com/xml.response"
	addressURL    = "https://api.ipify.org"
)

const (
	// maxCallsPerMinute keeps the plugin under Namecheap's limit of 20 calls a minute, counted
	// across every run of the member (the call log is in its workspace).
	maxCallsPerMinute = 18
	callLogFile       = "namecheap-calls.json"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

type client struct {
	p        *plugin
	clientIP string // found once per run
}

func newClient(p *plugin) *client { return &client{p: p} }

// apiError is an answer Namecheap gave with Status="ERROR", or no answer at all (Number 0).
type apiError struct {
	Number  string
	Message string
}

func (e *apiError) Error() string {
	if e.Number == "" {
		return e.Message
	}
	return fmt.Sprintf("Namecheap answered error %s: %s", e.Number, e.Message)
}

// accessProblem says whether the error means the API user or key is wrong or API access is off.
func (e *apiError) accessProblem() bool {
	switch e.Number {
	case "1010101", "1010102", "1011101", "1011102", "1017101", "1017102":
		return true
	}
	m := strings.ToLower(e.Message)
	return strings.Contains(m, "api key") || strings.Contains(m, "apikey") || strings.Contains(m, "api access") || strings.Contains(m, "apiuser")
}

// addressProblem says whether the error means this computer's address is not on the allow-list.
func (e *apiError) addressProblem() bool {
	return e.Number == "1011150" || strings.Contains(strings.ToLower(e.Message), "invalid request ip")
}

type apiResponse struct {
	Status string `xml:"Status,attr"`
	Errors []struct {
		Number  string `xml:"Number,attr"`
		Message string `xml:",chardata"`
	} `xml:"Errors>Error"`
	CommandResponse struct {
		Hosts struct {
			Domain        string    `xml:"Domain,attr"`
			EmailType     string    `xml:"EmailType,attr"`
			IsUsingOurDNS string    `xml:"IsUsingOurDNS,attr"`
			Hosts         []xmlHost `xml:"host"`
		} `xml:"DomainDNSGetHostsResult"`
		SetHosts struct {
			IsSuccess string `xml:"IsSuccess,attr"`
		} `xml:"DomainDNSSetHostsResult"`
		Domains []struct {
			Name     string `xml:"Name,attr"`
			IsOurDNS string `xml:"IsOurDNS,attr"`
		} `xml:"DomainGetListResult>Domain"`
		Paging struct {
			TotalItems  int `xml:"TotalItems"`
			CurrentPage int `xml:"CurrentPage"`
			PageSize    int `xml:"PageSize"`
		} `xml:"Paging"`
	} `xml:"CommandResponse"`
}

type xmlHost struct {
	Name    string `xml:"Name,attr"`
	Type    string `xml:"Type,attr"`
	Address string `xml:"Address,attr"`
	MXPref  string `xml:"MXPref,attr"`
	TTL     string `xml:"TTL,attr"`
}

// address is this computer's public IPv4 address, as Namecheap sees it: the connection's
// Whitelisted IP, else the clientIp setting, else what the address service answers.
func (c *client) address() (string, error) {
	if c.p.sec.ClientIP != "" {
		return c.p.sec.ClientIP, nil
	}
	if c.p.cfg.ClientIP != "" {
		return c.p.cfg.ClientIP, nil
	}
	if c.clientIP != "" {
		return c.clientIP, nil
	}
	fail := refusal("This computer's internet address could not be found from the address service; a person sets it as the Whitelisted IP of the namecheap connection in Team settings, Connections.")
	resp, err := httpClient.Get(addressURL)
	if err != nil {
		return "", fail
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	ip := net.ParseIP(strings.TrimSpace(string(body)))
	if resp.StatusCode != http.StatusOK || ip == nil || ip.To4() == nil {
		return "", fail
	}
	c.clientIP = ip.String()
	return c.clientIP, nil
}

// call sends one Namecheap command. The API key goes in a POST body, never in a URL, so no error
// the HTTP stack makes can carry it.
func (c *client) call(command string, params url.Values) (*apiResponse, error) {
	ip, err := c.address()
	if err != nil {
		return nil, err
	}
	c.p.waitForCallRoom()

	form := url.Values{}
	for k, v := range params {
		form[k] = v
	}
	form.Set("ApiUser", c.p.sec.APIUser)
	form.Set("ApiKey", c.p.sec.APIKey)
	form.Set("UserName", c.p.sec.APIUser)
	form.Set("ClientIp", ip)
	form.Set("Command", command)

	endpoint := productionURL
	if c.p.cfg.Sandbox {
		endpoint = sandboxURL
	}
	resp, err := httpClient.PostForm(endpoint, form)
	if err != nil {
		return nil, &apiError{Message: "Namecheap's API could not be reached."}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil, &apiError{Message: fmt.Sprintf("Namecheap's API answered HTTP %d.", resp.StatusCode)}
	}
	var r apiResponse
	if xml.Unmarshal(body, &r) != nil {
		return nil, &apiError{Message: "Namecheap's answer could not be read."}
	}
	if !strings.EqualFold(r.Status, "OK") {
		e := &apiError{Number: "unknown", Message: "no reason given"}
		if len(r.Errors) > 0 {
			e.Number = strings.TrimSpace(r.Errors[0].Number)
			e.Message = strings.TrimSpace(r.Errors[0].Message)
		}
		return nil, e
	}
	return &r, nil
}

// accountDomain is one domain of the account, from domains.getList.
type accountDomain struct {
	Name   string
	OurDNS bool
}

func (c *client) domains() ([]accountDomain, error) {
	var all []accountDomain
	for page := 1; page <= 50; page++ {
		r, err := c.call("namecheap.domains.getList", url.Values{"PageSize": {"100"}, "Page": {strconv.Itoa(page)}})
		if err != nil {
			return nil, err
		}
		for _, d := range r.CommandResponse.Domains {
			all = append(all, accountDomain{Name: normaliseDomain(d.Name), OurDNS: strings.EqualFold(d.IsOurDNS, "true")})
		}
		if len(r.CommandResponse.Domains) == 0 || len(all) >= r.CommandResponse.Paging.TotalItems {
			break
		}
	}
	return all, nil
}

// hostSet is a domain's whole record set as getHosts answers it.
type hostSet struct {
	EmailType string
	OurDNS    bool
	Records   []record
}

func splitDomain(domain string) (sld, tld string) {
	sld, tld, _ = strings.Cut(domain, ".")
	return sld, tld
}

func (c *client) getHosts(domain string) (*hostSet, error) {
	sld, tld := splitDomain(domain)
	r, err := c.call("namecheap.domains.dns.getHosts", url.Values{"SLD": {sld}, "TLD": {tld}})
	if err != nil {
		return nil, err
	}
	h := r.CommandResponse.Hosts
	set := &hostSet{EmailType: h.EmailType, OurDNS: strings.EqualFold(h.IsUsingOurDNS, "true"), Records: []record{}}
	for _, x := range h.Hosts {
		rec := record{Type: strings.ToUpper(x.Type), Host: normaliseHost(x.Name, domain), Value: x.Address}
		if rec.Type == "CAA" {
			rec.Value = caaValue(rec.Value)
		}
		rec.MXPref, _ = strconv.Atoi(x.MXPref)
		rec.TTL, _ = strconv.Atoi(x.TTL)
		if rec.Type != "MX" {
			rec.MXPref = 0
		}
		set.Records = append(set.Records, rec)
	}
	return set, nil
}

// setHosts writes the domain's whole record set: every record not in records is gone afterwards.
func (c *client) setHosts(domain, emailType string, records []record) error {
	sld, tld := splitDomain(domain)
	params := url.Values{"SLD": {sld}, "TLD": {tld}}
	if emailType != "" {
		params.Set("EmailType", emailType)
	}
	for i, r := range records {
		n := strconv.Itoa(i + 1)
		params.Set("HostName"+n, r.Host)
		params.Set("RecordType"+n, r.Type)
		value := r.Value
		if r.Type == "CAA" {
			value = caaValue(value) // a backup taken before CAA values were read in this form
		}
		params.Set("Address"+n, value)
		if r.TTL > 0 {
			params.Set("TTL"+n, strconv.Itoa(r.TTL))
		}
		if r.Type == "MX" {
			params.Set("MXPref"+n, strconv.Itoa(r.MXPref))
		}
	}
	r, err := c.call("namecheap.domains.dns.setHosts", params)
	if err != nil {
		return err
	}
	if !strings.EqualFold(r.CommandResponse.SetHosts.IsSuccess, "true") {
		return &apiError{Message: "Namecheap did not confirm the write (IsSuccess was not true)."}
	}
	return nil
}

// waitForCallRoom keeps the member under maxCallsPerMinute: it reads the times of the calls made
// in the last minute from the call log, waits when the minute is full, and logs this call.
func (p *plugin) waitForCallRoom() {
	path := filepath.Join(p.dir, callLogFile)
	var stamps []int64
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &stamps)
	}
	recent := func(now time.Time) []int64 {
		var keep []int64
		for _, s := range stamps {
			if now.Sub(time.UnixMilli(s)) < time.Minute {
				keep = append(keep, s)
			}
		}
		sort.Slice(keep, func(i, j int) bool { return keep[i] < keep[j] })
		return keep
	}
	now := p.time.now()
	stamps = recent(now)
	if len(stamps) >= maxCallsPerMinute {
		wait := time.UnixMilli(stamps[len(stamps)-maxCallsPerMinute]).Add(time.Minute).Sub(now) + 100*time.Millisecond
		p.progress(fmt.Sprintf("waiting %s to stay under Namecheap's limit of 20 calls a minute", wait.Round(time.Second)))
		p.time.sleep(wait)
		now = p.time.now()
		stamps = recent(now)
	}
	stamps = append(stamps, now.UnixMilli())
	if raw, err := json.Marshal(stamps); err == nil {
		_ = os.WriteFile(path, raw, 0o600)
	}
}
