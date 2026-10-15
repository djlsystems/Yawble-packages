package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const subAPIVersion = "2022-12-01"

func init() {
	register(
		command{words: []string{"whoami"}, run: (*plugin).whoami},
		command{words: []string{"list"}, usage: "[resource group]", run: (*plugin).list},
		command{words: []string{"show"}, usage: "<resource id>", run: (*plugin).show},
	)
}

type subscription struct {
	ID    string `json:"subscriptionId"`
	Name  string `json:"displayName"`
	State string `json:"state"`
}

func (p *plugin) whoami(args []string) (string, error) {
	if err := argsExactly(args, 0, 0, "whoami", "no arguments"); err != nil {
		return "", err
	}
	account := p.account
	if account == "" {
		account = "not signed in"
	}
	sub := "not set"
	if p.cfg.Subscription != "" {
		sub = p.cfg.Subscription
		if p.az.has(apiARM) {
			var s subscription
			if found, err := p.az.get("/subscriptions/"+p.cfg.Subscription+"?api-version="+subAPIVersion, &s); err != nil {
				sub += " (it cannot be read: " + err.Error() + ")"
			} else if !found {
				sub += " (this sign-in cannot see it)"
			} else {
				sub = fmt.Sprintf("%s (%s, %s)", s.Name, s.ID, s.State)
			}
		}
	}
	var apis []string
	for _, a := range []api{apiARM, apiSQL, apiStorage} {
		if p.az.has(a) {
			apis = append(apis, apiNames[a])
		}
	}
	or := func(list []string, none string) string {
		if len(list) == 0 {
			return none
		}
		return strings.Join(list, ", ")
	}
	budget := "none"
	if p.cfg.MonthlyBudget > 0 {
		budget = fmt.Sprintf("%g a month, alerts at 80%% and 100%% to %s", p.cfg.MonthlyBudget, or(p.cfg.BudgetEmails, "nobody (budgetEmails is empty)"))
	}
	lines := []string{
		"- account: " + account,
		"- tokens for: " + or(apis, "no API"),
		"- subscription: " + sub,
		"- team tag: " + tagOwner + ": " + p.team + ", " + tagPlugin + ": " + pluginID,
		"- regions: " + or(p.cfg.Regions, "none set"),
		"- mode: " + p.cfg.Mode,
		fmt.Sprintf("- allowDelete: %s", onOff(p.cfg.AllowDelete)),
		"- sharedSqlServers: " + or(p.cfg.SharedSQLServers, "none"),
		"- " + registriesSetting + ": " + or(p.cfg.SharedRegistries, "none"),
		"- monthlyBudget: " + budget,
		"- namePrefix: " + or(nonEmpty(p.cfg.NamePrefix), "none"),
	}
	if move := p.cfg.moveRegistries(); move != "" {
		lines = append(lines, "- "+move)
	}
	return "Azure member:\n" + strings.Join(lines, "\n"), nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

type resource struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Location string            `json:"location"`
	Tags     map[string]string `json:"tags"`
}

func (p *plugin) mark(tags map[string]string) string {
	if p.ownedBy(tags) {
		return " [owned]"
	}
	return ""
}

func (p *plugin) list(args []string) (string, error) {
	if err := argsExactly(args, 0, 1, "list", "an optional resource group"); err != nil {
		return "", err
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	var lines []string
	each := func(raw json.RawMessage) {
		var r resource
		if json.Unmarshal(raw, &r) == nil {
			lines = append(lines, fmt.Sprintf("- %s (%s, %s)%s: %s", r.Name, r.Type, r.Location, p.mark(r.Tags), r.ID))
		}
	}
	base := "/subscriptions/" + p.cfg.Subscription
	if len(args) == 0 {
		if err := p.az.list(base+"/resourcegroups?api-version="+rgAPIVersion, each); err != nil {
			return "", err
		}
		sort.Strings(lines)
		return fmt.Sprintf("Resource groups in %s (%d; [owned] carries this team's tags):\n%s", p.cfg.Subscription, len(lines), strings.Join(lines, "\n")), nil
	}
	rg := args[0]
	if err := p.az.list(base+"/resourceGroups/"+rg+"/resources?api-version="+rgAPIVersion, each); err != nil {
		if isNotFound(err) {
			return "", refusef("Refused: resource group %s does not exist in the subscription.", rg)
		}
		return "", err
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "Resource group " + rg + " holds no resources.", nil
	}
	return fmt.Sprintf("Resources in %s (%d):\n%s", rg, len(lines), strings.Join(lines, "\n")), nil
}

func (p *plugin) show(args []string) (string, error) {
	if err := argsExactly(args, 1, 1, "show", "<resource id>"); err != nil {
		return "", err
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	id := strings.TrimSuffix(args[0], "/")
	r, ok := parseID(id)
	if !ok {
		return "", refusef("Refused: %q is not an Azure resource id.", id)
	}
	version, err := p.apiVersionFor(r)
	if err != nil {
		return "", err
	}
	var body map[string]any
	found, err := p.az.get(id+"?api-version="+version, &body)
	if err != nil {
		return "", err
	}
	if !found {
		return "", refusef("Refused: %s does not exist, or this sign-in cannot read it.", id)
	}
	pretty, _ := json.MarshalIndent(withoutSecrets(body), "", "  ")
	tags, _ := body["tags"].(map[string]any)
	owned := map[string]string{}
	for k, v := range tags {
		owned[k], _ = v.(string)
	}
	state := "not owned by this team (read only)"
	if p.ownedBy(owned) {
		state = "owned by this team"
	}
	return fmt.Sprintf("%s, %s:\n%s", id, state, pretty), nil
}

// secretField names a field whose text could be a credential: a key, a password, a secret, a token
// or a connection string. show never prints its value.
var secretField = regexp.MustCompile(`(?i)(key|password|secret|token|connectionstring|sas)$`)

// withoutSecrets copies a resource's JSON with every string under a secret-named field replaced.
func withoutSecrets(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			if s, ok := item.(string); ok && s != "" && secretField.MatchString(k) {
				out[k] = "[hidden]"
				continue
			}
			out[k] = withoutSecrets(item)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = withoutSecrets(item)
		}
		return out
	}
	return v
}
