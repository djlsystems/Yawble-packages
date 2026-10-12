package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const dnsAPIVersion = "2018-05-01"

var zoneForm = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func init() {
	register(
		command{words: []string{"dns", "zone", "create"}, usage: "<resource group> <zone>", run: (*plugin).dnsZoneCreate},
		command{words: []string{"dns", "record", "set"}, usage: "<zone> <name> <A|AAAA|CNAME|TXT|MX> <value...> [ttl=<seconds>]", run: (*plugin).dnsRecordSet},
		command{words: []string{"dns", "record", "delete"}, usage: "<zone> <name> <A|AAAA|CNAME|TXT|MX>", run: (*plugin).dnsRecordDelete},
	)
}

func (p *plugin) dnsZoneCreate(args []string) (string, error) {
	if err := argsExactly(args, 2, 2, "dns zone create", "<resource group> <zone>"); err != nil {
		return "", err
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	rg, zone := args[0], strings.ToLower(strings.TrimSuffix(args[1], "."))
	if !zoneForm.MatchString(zone) {
		return "", refusef("Refused: %q is not a DNS zone name such as example.com.", zone)
	}
	id := rgPath(p.cfg.Subscription, rg) + "/providers/Microsoft.Network/dnsZones/" + zone
	if err := p.mustOwn(change{id: id, kind: changeCreate}); err != nil {
		return "", err
	}
	what := "create DNS zone " + zone + " in " + rg + ", tagged for this team"
	if !p.cfg.apply() {
		return p.whatif("dns zone create", id, what, map[string]int{"create": 1}), nil
	}
	var out struct {
		Properties struct {
			NameServers []string `json:"nameServers"`
		} `json:"properties"`
	}
	body := map[string]any{"location": "global", "tags": p.ownTags()}
	if _, err := p.az.arm(http.MethodPut, id+"?api-version="+dnsAPIVersion, body, &out); err != nil {
		return "", err
	}
	p.forget(id)
	p.applied("dns zone create", id, what, []string{id})
	ns := strings.Join(out.Properties.NameServers, ", ")
	if ns == "" {
		ns = "the four Azure name servers the zone shows in the portal"
	}
	return fmt.Sprintf("DNS zone %s is ready in %s. At the domain's registrar, set its name servers to %s.", zone, rg, ns), nil
}

// zoneID finds a zone by name in the subscription.
func (p *plugin) zoneID(zone string) (string, error) {
	zone = strings.ToLower(strings.TrimSuffix(zone, "."))
	var id string
	err := p.az.list("/subscriptions/"+p.cfg.Subscription+"/providers/Microsoft.Network/dnszones?api-version="+dnsAPIVersion, func(raw json.RawMessage) {
		var r resource
		if json.Unmarshal(raw, &r) == nil && strings.EqualFold(r.Name, zone) {
			id = r.ID
		}
	})
	if err != nil {
		return "", err
	}
	if id == "" {
		return "", refusef("Refused: there is no DNS zone %s in the subscription. Create it with dns zone create first.", zone)
	}
	return id, nil
}

var recordTypes = map[string]string{"A": "ARecords", "AAAA": "AAAARecords", "CNAME": "CNAMERecord", "TXT": "TXTRecords", "MX": "MXRecords"}

var recordName = regexp.MustCompile(`^(@|\*|(\*\.)?[A-Za-z0-9_]([A-Za-z0-9_-]{0,62})(\.[A-Za-z0-9_]([A-Za-z0-9_-]{0,62}))*)$`)

func (p *plugin) dnsRecordSet(args []string) (string, error) {
	if len(args) < 4 {
		return "", refusal("Refused: dns record set takes <zone> <name> <A|AAAA|CNAME|TXT|MX> <value...> [ttl=<seconds>]. Use @ for the zone itself.")
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	zone, name, typ := args[0], args[1], strings.ToUpper(args[2])
	values := args[3:]
	ttl := 3600
	if last := values[len(values)-1]; strings.HasPrefix(strings.ToLower(last), "ttl=") {
		n, err := strconv.Atoi(last[4:])
		if err != nil || n < 1 || n > 2147483647 {
			return "", refusef("Refused: %s is not a ttl in seconds.", last)
		}
		ttl, values = n, values[:len(values)-1]
	}
	_, ok := recordTypes[typ]
	if !ok {
		return "", refusef("Refused: %s is not a record type this member sets; it sets A, AAAA, CNAME, TXT and MX.", typ)
	}
	if !recordName.MatchString(name) {
		return "", refusef("Refused: %q is not a record name; use @ for the zone itself, or a name such as www.", name)
	}
	if len(values) == 0 {
		return "", refusal("Refused: dns record set needs at least one value.")
	}
	props, err := recordProps(typ, values, ttl)
	if err != nil {
		return "", err
	}

	zoneID, err := p.zoneID(zone)
	if err != nil {
		return "", err
	}
	id := zoneID + "/" + typ + "/" + name
	if err := p.mustOwn(change{id: id, kind: changeModify}); err != nil {
		return "", err
	}
	what := fmt.Sprintf("set %s record %s in %s to %s (ttl %d)", typ, name, zone, strings.Join(values, " "), ttl)
	if !p.cfg.apply() {
		return p.whatif("dns record set", id, what, map[string]int{"modify": 1}), nil
	}
	if _, err := p.az.arm(http.MethodPut, id+"?api-version="+dnsAPIVersion, map[string]any{"properties": props}, nil); err != nil {
		return "", err
	}
	p.applied("dns record set", id, what, []string{id})
	return fmt.Sprintf("Set %s record %s in %s to %s (ttl %d).", typ, name, zone, strings.Join(values, " "), ttl), nil
}

func (p *plugin) dnsRecordDelete(args []string) (string, error) {
	if err := argsExactly(args, 3, 3, "dns record delete", "<zone> <name> <A|AAAA|CNAME|TXT|MX>"); err != nil {
		return "", err
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	if !p.cfg.AllowDelete {
		return "", refusal("Refused: allowDelete is off, so this member deletes nothing, in whatif or apply. A person turns allowDelete on in this member's settings.")
	}
	zone, name, typ := args[0], args[1], strings.ToUpper(args[2])
	if _, ok := recordTypes[typ]; !ok {
		return "", refusef("Refused: %s is not a record type this member sets; it sets A, AAAA, CNAME, TXT and MX.", typ)
	}
	zoneID, err := p.zoneID(zone)
	if err != nil {
		return "", err
	}
	id := zoneID + "/" + typ + "/" + name
	if err := p.mustOwn(change{id: id, kind: changeDelete}); err != nil {
		return "", err
	}
	what := fmt.Sprintf("delete %s record %s in %s", typ, name, zone)
	if !p.cfg.apply() {
		return p.whatif("dns record delete", id, what, map[string]int{"delete": 1}), nil
	}
	if _, err := p.az.arm(http.MethodDelete, id+"?api-version="+dnsAPIVersion, nil, nil); err != nil && !isNotFound(err) {
		return "", err
	}
	p.applied("dns record delete", id, what, []string{id})
	return fmt.Sprintf("Deleted %s record %s in %s.", typ, name, zone), nil
}

// recordProps is the body of a DNS record set of one type holding the values.
func recordProps(typ string, values []string, ttl int) (map[string]any, error) {
	props := map[string]any{"TTL": ttl}
	field := recordTypes[typ]
	switch typ {
	case "A", "AAAA":
		var recs []map[string]string
		for _, v := range values {
			ip := net.ParseIP(v)
			if ip == nil || (typ == "A") != (ip.To4() != nil) {
				return nil, refusef("Refused: %s is not an %s address.", v, map[string]string{"A": "IPv4", "AAAA": "IPv6"}[typ])
			}
			key := map[string]string{"A": "ipv4Address", "AAAA": "ipv6Address"}[typ]
			recs = append(recs, map[string]string{key: v})
		}
		props[field] = recs
	case "CNAME":
		if len(values) != 1 {
			return nil, refusal("Refused: a CNAME record has exactly one value, a host name.")
		}
		props[field] = map[string]string{"cname": values[0]}
	case "TXT":
		props[field] = []map[string][]string{{"value": {strings.Join(values, " ")}}}
	case "MX":
		if len(values)%2 != 0 {
			return nil, refusal("Refused: an MX record's values come in pairs: <preference> <host>, such as 10 mail.example.com.")
		}
		var recs []map[string]any
		for i := 0; i < len(values); i += 2 {
			pref, err := strconv.Atoi(values[i])
			if err != nil || pref < 0 || pref > 65535 {
				return nil, refusef("Refused: %s is not an MX preference (0 to 65535).", values[i])
			}
			recs = append(recs, map[string]any{"preference": pref, "exchange": values[i+1]})
		}
		props[field] = recs
	}

	return props, nil
}
