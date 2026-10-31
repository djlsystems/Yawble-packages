package main

import (
	"errors"
	"fmt"
	"strings"
)

// setup checks each thing the plugin needs, in the order a person sets them up, and says the next
// step in words. It reads; it never changes anything.
func (p *plugin) setup() string {
	var out []string
	var next []string
	ok := func(s string) { out = append(out, "ok       "+s) }
	miss := func(s, step string) {
		out = append(out, "MISSING  "+s)
		next = append(next, step)
	}
	unchecked := func(s string) { out = append(out, "not yet  "+s) }

	keys := p.sec.APIUser != "" && p.sec.APIKey != ""
	if keys {
		ok("the namecheap connection holds an API user and API key")
	} else {
		miss("no Namecheap API user and API key are bound",
			"In Team settings, Connections, add a Namecheap API-key connection - API user (your Namecheap user name), API key (Profile, Tools, Namecheap API Access) and Whitelisted IP - assign it to this team and bind it to this member's `namecheap` slot.")
	}

	ip, ipErr := p.api.address()
	switch {
	case ipErr != nil:
		miss("this computer's internet address could not be found",
			"Set this computer's public IPv4 address as the Whitelisted IP of the namecheap connection in Team settings, Connections.")
	case p.sec.ClientIP != "":
		ok("this computer's internet address is " + ip + " (the namecheap connection's Whitelisted IP)")
	case p.cfg.ClientIP != "":
		ok("this computer's internet address is " + ip + " (from the clientIp setting)")
	default:
		ok("this computer's internet address is " + ip)
	}

	allowStep := func() string {
		if ip == "" {
			return "Add this computer's address to Namecheap's API allow-list (Profile, Tools, Namecheap API Access, Whitelisted IPs)."
		}
		return fmt.Sprintf("Add %s to Namecheap's API allow-list: Profile, Tools, Namecheap API Access, Whitelisted IPs, Edit, Add IP.", ip)
	}
	allowLine := func(state string) string {
		if ip == "" {
			return "this computer's address " + state
		}
		return ip + " " + state
	}

	var account []accountDomain
	apiWorks := false
	switch {
	case !keys:
		unchecked("API access is on for the account (checked once the key is set)")
		unchecked(allowLine("is on Namecheap's API allow-list (checked once the key is set)"))
	case ipErr != nil:
		unchecked("API access is on for the account (checked once the address is known)")
		unchecked("this computer's address is on Namecheap's API allow-list (checked once it is known)")
	default:
		var err error
		account, err = p.api.domains()
		var e *apiError
		switch {
		case err == nil:
			apiWorks = true
			ok("API access is on for the account")
			ok(allowLine("is on Namecheap's API allow-list"))
		case errors.As(err, &e) && e.addressProblem():
			ok("API access is on for the account")
			miss(allowLine("is not on Namecheap's API allow-list"), allowStep())
		case errors.As(err, &e) && e.accessProblem():
			miss("API access is off for the account, or the API user or key is wrong ("+errorWords(err)+")",
				"Turn API access on in Namecheap (Profile, Tools, Namecheap API Access, ON) and put that page's API key and your Namecheap user name in the namecheap connection (Team settings, Connections). The README says what Namecheap asks of an account before it turns API access on.")
			unchecked(allowLine("is on Namecheap's API allow-list (checked once API access works)"))
			next = append(next, allowStep())
		default:
			miss("Namecheap's API did not answer as expected ("+errorWords(err)+")",
				"Run setup again in a minute; if it says the same, check Namecheap's status page.")
		}
	}

	if len(p.cfg.Domains) == 0 {
		miss("no domains are listed",
			"List the domains this member may change in its domains setting, such as example.com.")
	}
	inAccount := map[string]accountDomain{}
	for _, d := range account {
		inAccount[d.Name] = d
	}
	for _, d := range p.cfg.Domains {
		a, found := inAccount[d]
		switch {
		case !apiWorks:
			unchecked(d + " is in the account and uses Namecheap DNS (checked once the API answers)")
		case !found:
			miss(d+" is not in this Namecheap account",
				fmt.Sprintf("Take %s out of the domains setting, or use the API user and key of the Namecheap account that holds it.", d))
		case !a.OurDNS:
			miss(d+" is in the account but does not use Namecheap DNS",
				fmt.Sprintf("In Namecheap, Domain List, %s, Manage, Nameservers, choose Namecheap BasicDNS - only if its records are meant to be served by Namecheap: switching moves where the domain's records come from.", d))
		default:
			ok(d + " is in the account and uses Namecheap DNS")
		}
	}

	if p.cfg.Mode == "apply" {
		out = append(out, "mode     apply: changes are made")
	} else {
		out = append(out, "mode     dryrun: changes are shown, not made; a person sets mode to apply to make them")
	}
	if p.cfg.Sandbox {
		out = append(out, "sandbox  on: every call goes to Namecheap's sandbox, with the sandbox account's user and key")
	}

	head := "Namecheap setup:"
	tail := "Ready: every check passed. `list <domain>` shows a domain's records."
	if len(next) > 0 {
		tail = "Next step: " + next[0]
		if len(next) > 1 {
			tail += "\nThen: " + strings.Join(next[1:], "\nThen: ")
		}
		tail += "\nRun setup again after each step."
	}
	return head + "\n" + strings.Join(out, "\n") + "\n" + tail
}
