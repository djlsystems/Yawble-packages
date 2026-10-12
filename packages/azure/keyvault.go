package main

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Key Vault: the plugin creates vaults that use Azure roles for access, and lists the names of the
// secrets in one. It never reads a secret's value: app secrets are set by a person or another plugin,
// and a container app reads them itself through a Key Vault reference.

const vaultDataVersion = "7.4"

var (
	vaultName     = regexp.MustCompile(`^[A-Za-z](?:[A-Za-z0-9]|-[A-Za-z0-9]){2,23}$`)
	secretRefForm = regexp.MustCompile(`^https://([A-Za-z0-9-]{3,24})\.vault\.azure\.net/secrets/([A-Za-z0-9-]{1,127})(?:/([0-9a-f]{32}))?/?$`)
)

func vaultHost(name string) string { return "https://" + strings.ToLower(name) + ".vault.azure.net" }

func init() {
	register(
		command{words: []string{"keyvault", "create"}, usage: "<resource group> <vault>", run: (*plugin).vaultCreate},
		command{words: []string{"keyvault", "secrets"}, usage: "<vault>", run: (*plugin).vaultSecrets},
	)
}

func (p *plugin) vaultCreate(args []string) (string, error) {
	if err := argsExactly(args, 2, 2, "keyvault create", "<resource group> <vault>"); err != nil {
		return "", err
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	rg, name := args[0], args[1]
	if !vaultName.MatchString(name) {
		return "", refusef("Refused: %q is not a Key Vault name: 3 to 24 letters, digits and single hyphens, starting with a letter, unique in all of Azure.", name)
	}
	id := rgPath(p.cfg.Subscription, rg) + "/providers/Microsoft.KeyVault/vaults/" + name
	if err := p.mustOwn(change{id: id, kind: changeCreate}); err != nil {
		return "", err
	}
	what := fmt.Sprintf("create Key Vault %s in %s, using Azure roles for access, tagged for this team", name, rg)
	if !p.cfg.apply() {
		return p.whatif("keyvault create", id, what, map[string]int{"create": 1}), nil
	}
	_, tenant, _, err := p.signedIn()
	if err != nil {
		return "", err
	}
	location, err := p.rgLocation(rg)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"location": location, "tags": p.ownTags(),
		"properties": map[string]any{
			"tenantId":                tenant,
			"sku":                     map[string]any{"family": "A", "name": "standard"},
			"enableRbacAuthorization": true,
			"enableSoftDelete":        true,
		},
	}
	if _, err := p.put(id, body, "the Key Vault "+name); err != nil {
		return "", err
	}
	p.applied("keyvault create", id, what, []string{id})
	return fmt.Sprintf("Key Vault %s is ready in %s. A person (or another plugin) sets its secrets, which needs the Key Vault Secrets Officer role on it; an app reads one through secret=<NAME>=%s/secrets/<name> on containerapp create.", name, rg, vaultHost(name)), nil
}

// secretNames lists the names of the secrets in a vault, never their values.
func (p *plugin) secretNames(vault string) ([]string, error) {
	var names []string
	next := vaultHost(vault) + "/secrets?maxresults=25&api-version=" + vaultDataVersion
	for page := 0; next != "" && page < 40; page++ {
		var out struct {
			Value []struct {
				ID         string `json:"id"`
				Attributes struct {
					Enabled bool `json:"enabled"`
				} `json:"attributes"`
			} `json:"value"`
			NextLink string `json:"nextLink"`
		}
		if _, err := p.az.data(apiVault, http.MethodGet, next, nil, nil, &out); err != nil {
			return nil, err
		}
		for _, s := range out.Value {
			name := s.ID[strings.LastIndex(s.ID, "/")+1:]
			if !s.Attributes.Enabled {
				name += " (disabled)"
			}
			names = append(names, name)
		}
		next = ""
		if u, err := url.Parse(out.NextLink); err == nil && strings.EqualFold(u.Host, strings.TrimPrefix(vaultHost(vault), "https://")) && u.Path == "/secrets" {
			next = out.NextLink
		}
	}
	sort.Strings(names)
	return names, nil
}

func (p *plugin) vaultSecrets(args []string) (string, error) {
	if err := argsExactly(args, 1, 1, "keyvault secrets", "<vault>"); err != nil {
		return "", err
	}
	if !vaultName.MatchString(args[0]) {
		return "", refusef("Refused: %q is not a Key Vault name.", args[0])
	}
	names, err := p.secretNames(args[0])
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return fmt.Sprintf("Key Vault %s holds no secrets.", args[0]), nil
	}
	return fmt.Sprintf("Secrets in Key Vault %s, by name only (%d):\n- %s", args[0], len(names), strings.Join(names, "\n- ")), nil
}
