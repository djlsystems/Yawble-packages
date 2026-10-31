package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The Twilio base URLs. Build-time only: tests change them, the shipped binary has no setting,
// flag or variable that points them anywhere else, so credentials cannot be redirected.
var (
	apiBase       = "https://api.twilio.com"
	messagingBase = "https://messaging.twilio.com"
	httpClient    = &http.Client{Timeout: 30 * time.Second}
	sleep         = time.Sleep
)

const (
	apiHost       = "api.twilio.com"
	messagingHost = "messaging.twilio.com"
	maxTries      = 3
)

// The restricted-key permissions each call needs, named in a 403.
const (
	permCreate    = "/twilio/messaging/messages/create"
	permRead      = "/twilio/messaging/messages/read"
	permList      = "/twilio/messaging/messages/list"
	permNumbers   = "/twilio/phone-numbers/active-numbers/list"
	permAccount   = "/twilio/iam/accounts/read"
	permCampaigns = "/twilio/messaging/services/usa2p-campaign/list"
	permTollFree  = "" // Twilio documents no restricted-key permission for it.
)

type client struct {
	account, keySid, keySecret string
}

// apiError is a Twilio answer other than 2xx, or no answer at all.
type apiError struct {
	Status      int
	Code        int
	Message     string
	Host        string
	Perm        string
	Tries       int
	Unreachable bool
}

func (e *apiError) Error() string {
	switch {
	case e.Unreachable:
		return fmt.Sprintf("Could not reach %s; try again later. Nothing was sent.", e.Host)
	case e.Status == http.StatusUnauthorized:
		return "Twilio rejected the Account SID, API key SID and API key secret of the connection bound to the `twilio` slot (401). A person replaces them in that connection in Team settings -> Connections, or makes a new key."
	case e.Status == http.StatusForbidden && e.Perm != "":
		return fmt.Sprintf("Twilio refused the request (403): the API key lacks the permission %s. A person adds it to the restricted key in the Twilio Console.", e.Perm)
	case e.Status == http.StatusTooManyRequests:
		return fmt.Sprintf("Twilio answered 429 Too Many Requests; tried %d times, nothing was sent. Try again later.", e.Tries)
	case e.Code != 0:
		return fmt.Sprintf("Twilio answered %d with %d: %s. %s", e.Status, e.Code, strings.TrimSuffix(e.Message, "."), errorDocs(e.Code))
	default:
		return fmt.Sprintf("Twilio answered %d; nothing was changed.", e.Status)
	}
}

func errorDocs(code int) string {
	return fmt.Sprintf("https://www.twilio.com/docs/api/errors/%d", code)
}

func (c *client) accountPath(rest string) string {
	return "/2010-04-01/Accounts/" + url.PathEscape(c.account) + rest
}

// do calls Twilio: basic auth with the API key, a form-encoded body for POST, JSON back into out.
// A 429 is tried again with backoff, up to 3 tries in all: Twilio did not process it, so even a
// POST is safe to repeat.
func (c *client) do(method, base, path string, form url.Values, perm string, out any) error {
	host := apiHost
	if base == messagingBase {
		host = messagingHost
	}
	target := base + path
	var body string
	if method == http.MethodGet && len(form) > 0 {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + form.Encode()
	} else if form != nil {
		body = form.Encode()
	}

	for try := 1; ; try++ {
		req, err := http.NewRequest(method, target, strings.NewReader(body))
		if err != nil {
			return &apiError{Unreachable: true, Host: host}
		}
		req.SetBasicAuth(c.keySid, c.keySecret)
		req.Header.Set("Accept", "application/json")
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return &apiError{Unreachable: true, Host: host}
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			if try < maxTries {
				sleep(time.Duration(try) * time.Second)
				continue
			}
			return &apiError{Status: 429, Host: host, Tries: try}
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			e := &apiError{Status: resp.StatusCode, Host: host, Perm: perm}
			var twilio struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			if json.Unmarshal(raw, &twilio) == nil {
				e.Code, e.Message = twilio.Code, twilio.Message
			}
			return e
		}
		if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				return &apiError{Status: resp.StatusCode, Host: host, Message: "an answer that is not JSON"}
			}
		}
		return nil
	}
}

// ---- Twilio resources -------------------------------------------------------------------------

type message struct {
	Sid          string  `json:"sid"`
	Direction    string  `json:"direction"`
	From         string  `json:"from"`
	To           string  `json:"to"`
	Body         string  `json:"body"`
	Status       string  `json:"status"`
	NumSegments  string  `json:"num_segments"`
	NumMedia     string  `json:"num_media"`
	Price        *string `json:"price"`
	PriceUnit    string  `json:"price_unit"`
	ErrorCode    *int    `json:"error_code"`
	ErrorMessage *string `json:"error_message"`
	DateCreated  string  `json:"date_created"`
	DateUpdated  string  `json:"date_updated"`
	DateSent     *string `json:"date_sent"`
}

type messagePage struct {
	Messages    []message `json:"messages"`
	NextPageURI *string   `json:"next_page_uri"`
}

type number struct {
	Sid          string `json:"sid"`
	PhoneNumber  string `json:"phone_number"`
	FriendlyName string `json:"friendly_name"`
	SmsURL       string `json:"sms_url"`
	Capabilities struct {
		SMS   bool `json:"sms"`
		MMS   bool `json:"mms"`
		Voice bool `json:"voice"`
	} `json:"capabilities"`
}

type account struct {
	Sid          string `json:"sid"`
	FriendlyName string `json:"friendly_name"`
	Type         string `json:"type"`
	Status       string `json:"status"`
}

func (c *client) sendMessage(form url.Values) (message, error) {
	var m message
	err := c.do(http.MethodPost, apiBase, c.accountPath("/Messages.json"), form, permCreate, &m)
	return m, err
}

func (c *client) fetchMessage(sid string) (message, error) {
	var m message
	err := c.do(http.MethodGet, apiBase, c.accountPath("/Messages/"+url.PathEscape(sid)+".json"), nil, permRead, &m)
	return m, err
}

// listMessages pages through Messages.json with the filters given, newest first, up to limit.
func (c *client) listMessages(filter url.Values, limit int) ([]message, error) {
	q := url.Values{}
	for k, v := range filter {
		q[k] = v
	}
	size := limit
	if size > 1000 {
		size = 1000
	}
	if size < 20 {
		size = 20
	}
	q.Set("PageSize", fmt.Sprint(size))
	var all []message
	path := c.accountPath("/Messages.json")
	form := q
	for {
		var page messagePage
		if err := c.do(http.MethodGet, apiBase, path, form, permList, &page); err != nil {
			return all, err
		}
		all = append(all, page.Messages...)
		if len(all) >= limit || page.NextPageURI == nil || *page.NextPageURI == "" {
			break
		}
		path, form = *page.NextPageURI, nil
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func (c *client) listNumbers(phone string) ([]number, error) {
	q := url.Values{"PageSize": {"100"}}
	if phone != "" {
		q.Set("PhoneNumber", phone)
	}
	var page struct {
		Numbers []number `json:"incoming_phone_numbers"`
	}
	err := c.do(http.MethodGet, apiBase, c.accountPath("/IncomingPhoneNumbers.json"), q, permNumbers, &page)
	return page.Numbers, err
}

func (c *client) fetchAccount() (account, error) {
	var a account
	err := c.do(http.MethodGet, apiBase, c.accountPath(".json"), nil, permAccount, &a)
	return a, err
}

func (c *client) fetchBalance() (string, string, error) {
	var b struct {
		Balance  string `json:"balance"`
		Currency string `json:"currency"`
	}
	err := c.do(http.MethodGet, apiBase, c.accountPath("/Balance.json"), nil, "", &b)
	return b.Balance, b.Currency, err
}

type campaign struct {
	Sid            string `json:"sid"`
	CampaignStatus string `json:"campaign_status"`
	DateCreated    string `json:"date_created"`
}

func (c *client) campaigns(service string) ([]campaign, error) {
	var page struct {
		Compliance []campaign `json:"compliance"`
	}
	err := c.do(http.MethodGet, messagingBase, "/v1/Services/"+url.PathEscape(service)+"/Compliance/Usa2p", nil, permCampaigns, &page)
	return page.Compliance, err
}

type tollFreeVerification struct {
	Sid             string  `json:"sid"`
	Status          string  `json:"status"`
	DateCreated     string  `json:"date_created"`
	RejectionReason *string `json:"rejection_reason"`
}

func (c *client) tollFreeVerifications(numberSid string) ([]tollFreeVerification, error) {
	var page struct {
		Verifications []tollFreeVerification `json:"verifications"`
	}
	err := c.do(http.MethodGet, messagingBase, "/v1/Tollfree/Verifications", url.Values{"TollfreePhoneNumberSid": {numberSid}}, permTollFree, &page)
	return page.Verifications, err
}

// twilioTime turns Twilio's RFC 1123 dates ("Thu, 09 Oct 2026 15:02:11 +0000") into UTC RFC 3339.
func twilioTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func isoTime(s string) string {
	if t, ok := twilioTime(s); ok {
		return t.Format(time.RFC3339)
	}
	return s
}

func (m message) date() string {
	if m.DateSent != nil && *m.DateSent != "" {
		return isoTime(*m.DateSent)
	}
	return isoTime(m.DateCreated)
}

func (m message) out() bool { return !strings.HasPrefix(m.Direction, "inbound") }

func (m message) code() int {
	if m.ErrorCode == nil {
		return 0
	}
	return *m.ErrorCode
}

func (m message) media() int {
	var n int
	fmt.Sscan(m.NumMedia, &n)
	return n
}

func finalStatus(s string) bool {
	switch s {
	case "delivered", "undelivered", "failed", "canceled", "received", "read":
		return true
	}
	return false
}
