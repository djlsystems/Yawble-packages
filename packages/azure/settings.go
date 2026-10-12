package main

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// settings are the manifest's config, every one set by a person only.
type settings struct {
	Subscription     string   // the subscription id, lower case, or "" until a person sets it
	Regions          []string // region names as Azure writes them: westeurope, eastus2
	Mode             string   // whatif or apply
	AllowDelete      bool
	SharedSQLServers []string // server names, lower case
	SharedRegistries []string // registry names, lower case
	MonthlyBudget    float64  // 0: no budget
	BudgetEmails     []string
	NamePrefix       string
}

var (
	guidForm    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	regionForm  = regexp.MustCompile(`^[a-z][a-z0-9]{2,40}$`)
	sqlName     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$`)
	acrName     = regexp.MustCompile(`^[a-z0-9]{5,50}$`)
	emailForm   = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	prefixForm  = regexp.MustCompile(`^[A-Za-z0-9_.()-]{0,40}$`)
	rgNameForm  = regexp.MustCompile(`^[A-Za-z0-9_.()-]{1,89}[A-Za-z0-9_()-]$|^[A-Za-z0-9_()-]$`)
	sqlServerID = regexp.MustCompile(`(?i)/providers/microsoft\.sql/servers/([^/]+)$`)
	acrID       = regexp.MustCompile(`(?i)/providers/microsoft\.containerregistry/registries/([^/]+)$`)
)

func parseSettings(raw map[string]json.RawMessage) (settings, string) {
	s := settings{Mode: "whatif"}

	str := func(name string) (string, bool) {
		v, ok := raw[name]
		if !ok || string(v) == "null" {
			return "", true
		}
		var out string
		if json.Unmarshal(v, &out) != nil {
			return "", false
		}
		return strings.TrimSpace(out), true
	}
	list := func(name string) ([]string, bool) {
		v, ok := raw[name]
		if !ok || string(v) == "null" {
			return nil, true
		}
		var out []string
		if json.Unmarshal(v, &out) != nil {
			return nil, false
		}
		var kept []string
		for _, item := range out {
			if item = strings.TrimSpace(item); item != "" {
				kept = append(kept, item)
			}
		}
		return kept, true
	}

	sub, ok := str("subscription")
	sub = strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(sub, "/subscriptions/"), "subscriptions/"))
	if !ok || (sub != "" && !guidForm.MatchString(sub)) {
		return s, "Refused: subscription must be a subscription id, 36 letters, digits and hyphens such as 00000000-0000-0000-0000-000000000000. Run setup to see the subscriptions this sign-in can read."
	}
	s.Subscription = sub

	regions, ok := list("regions")
	if !ok {
		return s, "Refused: regions must be a list of Azure region names, such as westeurope."
	}
	for _, r := range regions {
		name := strings.ToLower(strings.ReplaceAll(r, " ", ""))
		if !regionForm.MatchString(name) {
			return s, fmt.Sprintf("Refused: regions holds %q, which is not an Azure region name such as westeurope or eastus2.", r)
		}
		s.Regions = append(s.Regions, name)
	}

	mode, ok := str("mode")
	switch {
	case !ok || (mode != "" && mode != "whatif" && mode != "apply"):
		return s, "Refused: mode must be whatif or apply; a person sets it in this member's settings."
	case mode != "":
		s.Mode = mode
	}

	if v, ok := raw["allowDelete"]; ok && string(v) != "null" {
		if json.Unmarshal(v, &s.AllowDelete) != nil {
			return s, "Refused: allowDelete must be on or off."
		}
	}

	servers, ok := list("sharedSqlServers")
	if !ok {
		return s, "Refused: sharedSqlServers must be a list of SQL server names."
	}
	for _, v := range servers {
		name, good := lastName(v, sqlServerID, ".database.windows.net")
		if !good || !sqlName.MatchString(name) {
			return s, fmt.Sprintf("Refused: sharedSqlServers holds %q, which is not a SQL server name (such as my-sql-server), its address or its resource id.", v)
		}
		s.SharedSQLServers = append(s.SharedSQLServers, name)
	}

	registries, ok := list("sharedRegistries")
	if !ok {
		return s, "Refused: sharedRegistries must be a list of container registry names."
	}
	for _, v := range registries {
		name, good := lastName(v, acrID, ".azurecr.io")
		if !good || !acrName.MatchString(name) {
			return s, fmt.Sprintf("Refused: sharedRegistries holds %q, which is not a container registry name (5 to 50 letters and digits, such as myregistry), its address or its resource id.", v)
		}
		s.SharedRegistries = append(s.SharedRegistries, name)
	}

	if v, ok := raw["monthlyBudget"]; ok && string(v) != "null" {
		if json.Unmarshal(v, &s.MonthlyBudget) != nil || s.MonthlyBudget < 0 || math.IsInf(s.MonthlyBudget, 0) {
			return s, "Refused: monthlyBudget must be a number of the subscription's currency, 0 or more; 0 means no budget."
		}
	}

	emails, ok := list("budgetEmails")
	if !ok {
		return s, "Refused: budgetEmails must be a list of email addresses."
	}
	for _, e := range emails {
		if !emailForm.MatchString(e) {
			return s, fmt.Sprintf("Refused: budgetEmails holds %q, which is not an email address.", e)
		}
	}
	s.BudgetEmails = emails

	prefix, ok := str("namePrefix")
	if !ok || !prefixForm.MatchString(prefix) {
		return s, "Refused: namePrefix may hold only letters, digits, hyphens, underscores, periods and parentheses, at most 40 of them."
	}
	s.NamePrefix = prefix
	return s, ""
}

// lastName reads a shared resource given as its name, its address or its resource id, and answers
// its name in lower case.
func lastName(v string, idForm *regexp.Regexp, suffix string) (string, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	if strings.HasPrefix(v, "/subscriptions/") {
		m := idForm.FindStringSubmatch(v)
		if m == nil {
			return "", false
		}
		return m[1], true
	}
	v = strings.TrimPrefix(v, "https://")
	v = strings.TrimSuffix(v, "/")
	return strings.TrimSuffix(v, suffix), true
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if strings.EqualFold(item, v) {
			return true
		}
	}
	return false
}

func (s settings) sharedSQL(server string) bool { return contains(s.SharedSQLServers, server) }

func (s settings) sharedRegistry(name string) bool { return contains(s.SharedRegistries, name) }

func (s settings) apply() bool { return s.Mode == "apply" }
