package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// settings are the manifest's config, every one set by a person only.
type settings struct {
	Mode            string   // dryrun or apply
	KeyVaults       []string // vault names, lower case
	ExpectedAccount string   // acct_..., or "" until a person sets it
}

var (
	vaultName   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,22}[a-z0-9]$`)
	accountName = regexp.MustCompile(`^acct_[A-Za-z0-9]{1,64}$`)
)

func parseSettings(raw map[string]json.RawMessage) (settings, string) {
	s := settings{Mode: "dryrun"}

	if v, ok := raw["mode"]; ok {
		var mode string
		if json.Unmarshal(v, &mode) != nil || (mode != "dryrun" && mode != "apply") {
			return s, "Refused: mode must be dryrun or apply; a person sets it in this member's settings."
		}
		s.Mode = mode
	}

	if v, ok := raw["keyVaults"]; ok && string(v) != "null" {
		var list []string
		if json.Unmarshal(v, &list) != nil {
			return s, "Refused: keyVaults must be a list of Key Vault names."
		}
		for _, item := range list {
			name, ok := normaliseVault(item)
			if !ok {
				return s, fmt.Sprintf("Refused: keyVaults holds %q, which is not a Key Vault name (3 to 24 letters, digits and hyphens) or address (https://<name>.vault.azure.net).", item)
			}
			s.KeyVaults = append(s.KeyVaults, name)
		}
	}

	if v, ok := raw["expectedAccount"]; ok {
		var acct string
		if json.Unmarshal(v, &acct) != nil {
			return s, "Refused: expectedAccount must be a Stripe account id (acct_...)."
		}
		acct = strings.TrimSpace(acct)
		if acct != "" && !accountName.MatchString(acct) {
			return s, fmt.Sprintf("Refused: expectedAccount %q is not a Stripe account id (acct_...). Run setup to see the key's account.", acct)
		}
		s.ExpectedAccount = acct
	}
	return s, ""
}

// normaliseVault reads a vault as its name or its address and answers the name in lower case.
func normaliseVault(v string) (string, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.TrimPrefix(v, "https://")
	v = strings.TrimSuffix(v, "/")
	v = strings.TrimSuffix(v, ".vault.azure.net")
	return v, vaultName.MatchString(v)
}

func (s settings) vaultAllowed(name string) bool {
	for _, v := range s.KeyVaults {
		if v == name {
			return true
		}
	}
	return false
}
