package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// providers are the resource providers the plugin's commands use. setup checks each is registered
// on the subscription, and registers it in apply mode when the person's role allows.
var providers = []string{
	"Microsoft.App",
	"Microsoft.OperationalInsights",
	"Microsoft.Storage",
	"Microsoft.KeyVault",
	"Microsoft.Network",
	"Microsoft.ContainerRegistry",
	"Microsoft.Sql",
	"Microsoft.ManagedIdentity",
	"Microsoft.Consumption",
	"Microsoft.Insights",
}

const (
	sqlAPIVersion = "2023-08-01"
	acrAPIVersion = "2023-07-01"
)

func init() {
	register(command{words: []string{"setup"}, run: (*plugin).setup})
}

// permissions are the person's effective actions on the subscription, from every role they hold.
type permissions []struct {
	Actions    []string `json:"actions"`
	NotActions []string `json:"notActions"`
}

// allows reports whether any of the person's roles grants the action.
func (ps permissions) allows(action string) bool {
	for _, p := range ps {
		granted := false
		for _, a := range p.Actions {
			if actionMatch(a, action) {
				granted = true
				break
			}
		}
		for _, a := range p.NotActions {
			if actionMatch(a, action) {
				granted = false
				break
			}
		}
		if granted {
			return true
		}
	}
	return false
}

// actionMatch matches an Azure action pattern, where * stands for anything, in any case.
func actionMatch(pattern, action string) bool {
	parts := strings.Split(pattern, "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	re, err := regexp.Compile("(?i)^" + strings.Join(parts, ".*") + "$")
	return err == nil && re.MatchString(action)
}

func (p *plugin) setup(args []string) (string, error) {
	if err := argsExactly(args, 0, 0, "setup", "no arguments"); err != nil {
		return "", err
	}
	var lines, next []string
	say := func(line string) { lines = append(lines, "- "+line) }
	step := func(s string) { next = append(next, s) }
	finish := func() (string, error) {
		head := "Setup, mode " + p.cfg.Mode + ":\n" + strings.Join(lines, "\n")
		if len(next) == 0 {
			return head + "\nReady: every check passed. Changes happen only in mode apply, which a person sets in this member's settings.", nil
		}
		var steps []string
		for i, s := range next {
			steps = append(steps, fmt.Sprintf("%d. %s", i+1, s))
		}
		return head + "\nNext:\n" + strings.Join(steps, "\n"), nil
	}

	// SIGNED IN.
	if !p.az.has(apiARM) {
		say("signed in: no")
		step("Sign in with Microsoft on this member's microsoft connection slot (Member settings, Connections): open the link it shows, enter the code, and accept every API it asks for. Then run setup again.")
		return finish()
	}
	account := p.account
	if account == "" {
		account = "an account"
	}
	say("signed in: yes, as " + account)
	var missing []string
	for _, a := range []api{apiSQL, apiStorage} {
		if !p.az.has(a) {
			missing = append(missing, apiNames[a])
		}
	}
	if len(missing) > 0 {
		say("tokens: none for " + strings.Join(missing, ", "))
		step("Reconnect the Microsoft connection in Admin, Connections and accept " + strings.Join(missing, ", ") + "; the commands that use them fail until then.")
	} else {
		say("tokens: Azure management, Azure SQL and Azure Storage")
	}

	// THE SUBSCRIPTION.
	if p.cfg.Subscription == "" {
		var subs []string
		err := p.az.list("/subscriptions?api-version="+subAPIVersion, func(raw json.RawMessage) {
			var s subscription
			if json.Unmarshal(raw, &s) == nil && s.ID != "" {
				subs = append(subs, fmt.Sprintf("%s (%s)", s.ID, s.Name))
			}
		})
		switch {
		case err != nil:
			say("subscription: not set, and the subscriptions could not be listed: " + err.Error())
			step("Put the subscription id (Azure portal, Subscriptions) in this member's subscription setting, then run setup again.")
		case len(subs) == 0:
			say("subscription: not set, and this sign-in can read no subscription")
			step("Ask the subscription's owner to give this account a role on it (Contributor lets it create resource groups), or create a subscription in the Azure portal, then put its id in this member's subscription setting.")
		default:
			sort.Strings(subs)
			say("subscription: not set; this sign-in can read " + strings.Join(subs, ", "))
			step("Put one of these subscription ids in this member's subscription setting, then run setup again: " + strings.Join(subs, ", ") + ".")
		}
		return finish()
	}
	var sub subscription
	found, err := p.az.get("/subscriptions/"+p.cfg.Subscription+"?api-version="+subAPIVersion, &sub)
	switch {
	case err != nil && !isStatus(err, http.StatusForbidden):
		say("subscription " + p.cfg.Subscription + ": could not be read: " + err.Error())
		step("Try setup again; if it keeps failing, check the subscription id in this member's settings.")
		return finish()
	case !found || err != nil:
		say("subscription " + p.cfg.Subscription + ": this sign-in cannot read it")
		step("Check the subscription id in this member's settings, or ask the subscription's owner to give this account a role on it (Contributor lets it create resource groups).")
		return finish()
	case !strings.EqualFold(sub.State, "Enabled"):
		say(fmt.Sprintf("subscription: %s (%s) is %s", sub.Name, sub.ID, sub.State))
		step("The subscription must be enabled: its owner reactivates it in the Azure portal, Subscriptions.")
	default:
		say(fmt.Sprintf("subscription: %s (%s), readable", sub.Name, sub.ID))
	}
	base := "/subscriptions/" + p.cfg.Subscription

	// THE ROLE.
	var perms struct {
		Value permissions `json:"value"`
	}
	if _, err := p.az.arm(http.MethodGet, base+"/providers/Microsoft.Authorization/permissions?api-version="+roleAPIVersion, nil, &perms); err != nil {
		say("role: could not be read: " + err.Error())
		step("Try setup again; the role on the subscription decides what this member can do.")
	} else if perms.Value.allows("Microsoft.Resources/subscriptions/resourceGroups/write") {
		say("role: allows creating resource groups")
	} else {
		say("role: does not allow creating resource groups")
		step("Ask the subscription's owner to give this account the Contributor role on the subscription (Azure portal, Subscriptions, Access control (IAM), Add role assignment), then run setup again.")
	}

	// THE REGIONS.
	if len(p.cfg.Regions) == 0 {
		say("regions: none set")
		step("Put the regions resource groups may be created in, such as westeurope, in this member's regions setting.")
	} else {
		known := map[string]bool{}
		err := p.az.list(base+"/locations?api-version="+subAPIVersion, func(raw json.RawMessage) {
			var l struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(raw, &l) == nil {
				known[strings.ToLower(l.Name)] = true
			}
		})
		var unknown []string
		for _, r := range p.cfg.Regions {
			if err == nil && !known[r] {
				unknown = append(unknown, r)
			}
		}
		if len(unknown) > 0 {
			say("regions: " + strings.Join(unknown, ", ") + " not offered to this subscription")
			step("Correct regions in this member's settings: " + strings.Join(unknown, ", ") + " is not a region this subscription offers (the Azure portal lists them when you create a resource group).")
		} else {
			say("regions: " + strings.Join(p.cfg.Regions, ", "))
		}
	}

	// THE SHARED SQL SERVERS AND REGISTRIES.
	p.checkShared(say, step, "SQL server", "sharedSqlServers", p.cfg.SharedSQLServers,
		base+"/providers/Microsoft.Sql/servers?api-version="+sqlAPIVersion)
	p.checkShared(say, step, "registry", "sharedRegistries", p.cfg.SharedRegistries,
		base+"/providers/Microsoft.ContainerRegistry/registries?api-version="+acrAPIVersion)

	// THE PROVIDERS.
	canRegister := perms.Value.allows("Microsoft.App/register/action")
	var registered, registering, waiting []string
	for _, ns := range providers {
		var prov struct {
			RegistrationState string `json:"registrationState"`
		}
		if _, err := p.az.get(base+"/providers/"+ns+"?api-version="+rgAPIVersion, &prov); err != nil {
			say("provider " + ns + ": could not be read: " + err.Error())
			continue
		}
		switch state := strings.ToLower(prov.RegistrationState); {
		case state == "registered":
			registered = append(registered, ns)
		case state == "registering":
			registering = append(registering, ns)
		default:
			waiting = append(waiting, ns)
		}
	}
	if len(registered) > 0 {
		say("providers registered: " + strings.Join(registered, ", "))
	}
	if len(registering) > 0 {
		say("providers registering: " + strings.Join(registering, ", ") + " (a few minutes)")
		step("Wait a few minutes for " + strings.Join(registering, ", ") + " to finish registering, then run setup again.")
	}
	if len(waiting) > 0 {
		list := strings.Join(waiting, ", ")
		switch {
		case !canRegister:
			say("providers not registered: " + list + "; this account's role cannot register them")
			step("Ask the subscription's owner to register " + list + " (Azure portal, Subscriptions, Resource providers, Register), then run setup again.")
		case !p.cfg.apply():
			say("providers not registered: " + list + "; whatif registers nothing")
			p.planned(plan{command: "setup", target: p.cfg.Subscription, summary: "register " + list, counts: map[string]int{"create": len(waiting)}})
			step("Register " + list + ": a person sets mode to apply and runs setup again, which registers them, or registers them in the Azure portal (Subscriptions, Resource providers).")
		default:
			var done, failed []string
			for _, ns := range waiting {
				if _, err := p.az.arm(http.MethodPost, base+"/providers/"+ns+"/register?api-version="+rgAPIVersion, nil, nil); err != nil {
					failed = append(failed, ns+" ("+err.Error()+")")
					continue
				}
				done = append(done, ns)
			}
			if len(done) > 0 {
				say("providers registering now: " + strings.Join(done, ", "))
				p.applied("setup", p.cfg.Subscription, "registered "+strings.Join(done, ", "), []string{})
				step("Wait a few minutes for " + strings.Join(done, ", ") + " to finish registering, then run setup again.")
			}
			if len(failed) > 0 {
				say("providers that could not be registered: " + strings.Join(failed, "; "))
				step("Ask the subscription's owner to register them in the Azure portal (Subscriptions, Resource providers).")
			}
		}
	}

	// THE BUDGET.
	if p.cfg.MonthlyBudget > 0 && len(p.cfg.BudgetEmails) == 0 {
		say(fmt.Sprintf("budget: %g a month, but budgetEmails is empty", p.cfg.MonthlyBudget))
		step("Put the addresses that get the budget alerts in budgetEmails, or set monthlyBudget to 0 for no budget.")
	}
	return finish()
}

// checkShared says whether each listed shared resource is in the subscription and readable.
func (p *plugin) checkShared(say, step func(string), kind, setting string, names []string, listPath string) {
	if len(names) == 0 {
		say(setting + ": none listed")
		return
	}
	found := map[string]bool{}
	err := p.az.list(listPath, func(raw json.RawMessage) {
		var r resource
		if json.Unmarshal(raw, &r) == nil {
			found[strings.ToLower(r.Name)] = true
		}
	})
	if err != nil {
		say(setting + ": could not be listed: " + err.Error())
		step("Try setup again; this account needs at least Reader on each " + kind + " in " + setting + ".")
		return
	}
	var ok, missing []string
	for _, n := range names {
		if found[n] {
			ok = append(ok, n)
		} else {
			missing = append(missing, n)
		}
	}
	if len(ok) > 0 {
		say(setting + " reachable: " + strings.Join(ok, ", "))
	}
	if len(missing) > 0 {
		say(setting + " not reachable: " + strings.Join(missing, ", "))
		step(fmt.Sprintf("Check %s in this member's settings: this sign-in cannot see the %s %s in the subscription. Correct the name, or ask its owner to give this account a role on it.", setting, kind, strings.Join(missing, ", ")))
	}
}
