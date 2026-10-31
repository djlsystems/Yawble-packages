package main

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strings"
)

// settings are the manifest's config, with the manifest's defaults applied again here so a request
// that leaves a field out still gets the safe value: dry run, and no domain at all.
type settings struct {
	Domains  []string // lower case, no trailing dot
	Mode     string   // dryrun or apply
	ClientIP string   // empty: found from the address service
	Sandbox  bool
	// DefaultTTL is the ttl ensure gives a record it adds when the command names none.
	DefaultTTL int
}

// secrets are the namecheap connection's fields. ClientIP is its Whitelisted IP, "" when the
// connection leaves it empty.
type secrets struct {
	APIUser, APIKey, ClientIP string
}

// noKeys is what a run says when the namecheap slot has no connection, or one without a user or key.
const noKeys = "a person binds a Namecheap API-key connection (API user, API key, Whitelisted IP) to this member's `namecheap` slot; the connection is added and assigned to the team in Team settings, Connections"

var domainPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,62}$`)

func normaliseDomain(d string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
}

func parseSettings(raw map[string]json.RawMessage) (settings, string) {
	s := settings{Domains: []string{}, Mode: "dryrun", DefaultTTL: automaticTTL}

	var problems []string
	str := func(name string, into *string) {
		if v, ok := raw[name]; ok && string(v) != "null" {
			if json.Unmarshal(v, into) != nil {
				problems = append(problems, fmt.Sprintf("`%s` must be text", name))
			}
			*into = strings.TrimSpace(*into)
		}
	}

	str("mode", &s.Mode)
	s.Mode = strings.ToLower(s.Mode)
	str("clientIp", &s.ClientIP)
	if v, ok := raw["sandbox"]; ok && string(v) != "null" {
		if json.Unmarshal(v, &s.Sandbox) != nil {
			problems = append(problems, "`sandbox` must be on or off")
		}
	}
	if v, ok := raw["defaultTtl"]; ok && string(v) != "null" {
		var f float64
		if json.Unmarshal(v, &f) != nil || f != float64(int(f)) || int(f) < minTTL || int(f) > maxTTL {
			problems = append(problems, fmt.Sprintf("`defaultTtl` must be a whole number from %d to %d", minTTL, maxTTL))
		} else {
			s.DefaultTTL = int(f)
		}
	}
	if v, ok := raw["domains"]; ok && string(v) != "null" {
		var items []string
		if json.Unmarshal(v, &items) != nil {
			problems = append(problems, "`domains` must be a list")
		}
		for _, item := range items {
			d := normaliseDomain(item)
			if d == "" {
				continue
			}
			if !domainPattern.MatchString(d) {
				problems = append(problems, fmt.Sprintf("`domains` holds %q, which is not a domain name such as example.com", item))
				continue
			}
			s.Domains = append(s.Domains, d)
		}
	}

	if s.Mode != "dryrun" && s.Mode != "apply" {
		problems = append(problems, "`mode` must be dryrun or apply")
	}
	if s.ClientIP != "" {
		if ip := net.ParseIP(s.ClientIP); ip == nil || ip.To4() == nil {
			problems = append(problems, "`clientIp` must be an IPv4 address such as 203.0.113.7, or empty to find it")
		}
	}

	if len(problems) > 0 {
		return s, "This member's settings cannot be used: " + strings.Join(problems, "; ") + ". A person corrects them in this member's settings; nothing was changed."
	}
	return s, ""
}

func (s settings) listed(domain string) bool {
	for _, d := range s.Domains {
		if d == domain {
			return true
		}
	}
	return false
}
