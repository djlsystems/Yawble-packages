package main

import (
	"fmt"
	"net/http"
	"strings"
)

const budgetAPIVersion = "2021-10-01"

// budgetName is the budget the plugin keeps on each resource group it creates.
const budgetName = "yawble-monthly"

func init() {
	register(command{words: []string{"rg", "create"}, usage: "<name> <region>", run: (*plugin).rgCreate})
}

func (p *plugin) rgCreate(args []string) (string, error) {
	if err := argsExactly(args, 2, 2, "rg create", "<name> <region>"); err != nil {
		return "", err
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	name := p.cfg.NamePrefix + args[0]
	region := strings.ToLower(strings.ReplaceAll(args[1], " ", ""))
	if !rgNameForm.MatchString(name) {
		return "", refusef("Refused: %q is not a resource group name: letters, digits, hyphens, underscores, periods and parentheses, at most 90, not ending in a period.", name)
	}
	if !contains(p.cfg.Regions, region) {
		return "", refusef("Refused: %s is not in this member's regions (%s). A person adds it to the regions setting.", region, strings.Join(p.cfg.Regions, ", "))
	}
	if p.cfg.MonthlyBudget > 0 && len(p.cfg.BudgetEmails) == 0 {
		return "", refusal("Refused: monthlyBudget is set and budgetEmails is empty, so the budget's alerts would reach nobody. A person adds addresses to budgetEmails, or sets monthlyBudget to 0.")
	}
	id := rgPath(p.cfg.Subscription, name)
	if err := p.mustOwn(change{id: id, kind: changeCreate}); err != nil {
		return "", err
	}
	existing, err := p.tagsOf(id)
	if err != nil {
		return "", err
	}

	what := fmt.Sprintf("create resource group %s in %s, tagged %s: %s and %s: %s", name, region, tagOwner, p.team, tagPlugin, pluginID)
	counts := map[string]int{"create": 1}
	if existing != nil {
		what = "keep resource group " + name + ", which this team already owns"
		counts = map[string]int{"unchanged": 1}
	}
	if p.cfg.MonthlyBudget > 0 {
		what += fmt.Sprintf(", with a budget of %g a month alerting %s at 80%% and 100%%", p.cfg.MonthlyBudget, strings.Join(p.cfg.BudgetEmails, ", "))
		counts["create"]++
	}
	if !p.cfg.apply() {
		return p.whatif("rg create", id, what, counts), nil
	}

	ids := []string{}
	if existing == nil {
		body := map[string]any{"location": region, "tags": p.ownTags()}
		if _, err := p.az.arm(http.MethodPut, id+"?api-version="+rgAPIVersion, body, nil); err != nil {
			return "", err
		}
		p.forget(id)
		ids = append(ids, id)
	}
	answer := "Resource group " + name + " is ready in " + region + ", owned by this team"
	if p.cfg.MonthlyBudget > 0 {
		budget, err := p.putBudget(id)
		if err != nil {
			p.applied("rg create", id, what, ids)
			return "", fmt.Errorf("resource group %s is ready, but its budget could not be set: %w", name, err)
		}
		ids = append(ids, budget)
		answer += fmt.Sprintf(", with a budget of %g a month (alerts at 80%% and 100%%)", p.cfg.MonthlyBudget)
	}
	p.applied("rg create", id, what, ids)
	return answer + ".", nil
}

// putBudget sets the monthly budget on a resource group, alerting at 80% and 100% of actual cost.
func (p *plugin) putBudget(rg string) (string, error) {
	id := rg + "/providers/Microsoft.Consumption/budgets/" + budgetName
	start := p.now().UTC()
	alert := func(threshold int) map[string]any {
		return map[string]any{
			"enabled": true, "operator": "GreaterThanOrEqualTo", "threshold": threshold,
			"thresholdType": "Actual", "contactEmails": p.cfg.BudgetEmails,
		}
	}
	body := map[string]any{"properties": map[string]any{
		"category": "Cost", "amount": p.cfg.MonthlyBudget, "timeGrain": "Monthly",
		"timePeriod": map[string]any{"startDate": fmt.Sprintf("%04d-%02d-01T00:00:00Z", start.Year(), int(start.Month()))},
		"notifications": map[string]any{
			"actual80":  alert(80),
			"actual100": alert(100),
		},
	}}
	_, err := p.az.arm(http.MethodPut, id+"?api-version="+budgetAPIVersion, body, nil)
	return id, err
}
