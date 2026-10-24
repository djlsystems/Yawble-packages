package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Key Vault: the plugin creates vaults that use Azure roles for access, and lists the names of the
// secrets in one through Azure management, which answers names and attributes and never a value.
// It never reads a secret's value: app secrets are set by a person or another plugin, and a
// container app reads them itself through a Key Vault reference.

var (
	vaultName     = regexp.MustCompile(`^[A-Za-z](?:[A-Za-z0-9]|-[A-Za-z0-9]){2,23}$`)
	secretRefForm = regexp.MustCompile(`^https://([A-Za-z0-9-]{3,24})\.vault\.azure\.net/secrets/([A-Za-z0-9-]{1,127})(?:/([0-9a-f]{32}))?/?$`)
)

// vaultAddress is the address an app's Key Vault reference names a vault by. The plugin never
// calls it; Azure reads it with the app's own identity.
func vaultAddress(name string) string { return "https://" + strings.ToLower(name) + ".vault.azure.net" }

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
	if _, _, err := p.put(id, body, "the Key Vault "+name); err != nil {
		return "", err
	}
	p.applied("keyvault create", id, what, []string{id})
	return fmt.Sprintf("Key Vault %s is ready in %s. A person (or another plugin) sets its secrets, which needs the Key Vault Secrets Officer role on it; an app reads one through secret=<NAME>=%s/secrets/<name> on containerapp create.", name, rg, vaultAddress(name)), nil
}

// secretNames lists the names of the secrets in a vault through Azure management, never their
// values: the vault's secrets list answers names and attributes only. A disabled secret is named
// as such. Only nextLinks that stay on Azure management under the vault's own path are followed.
func (p *plugin) secretNames(vault resource) ([]string, error) {
	var names []string
	under := strings.ToLower(vault.ID) + "/secrets"
	next := armBase + vault.ID + "/secrets?api-version=" + versionOf("Microsoft.KeyVault/vaults")
	for page := 0; next != "" && page < 40; page++ {
		var out struct {
			Value []struct {
				Name       string `json:"name"`
				Properties struct {
					Attributes struct {
						Enabled *bool `json:"enabled"`
					} `json:"attributes"`
				} `json:"properties"`
			} `json:"value"`
			NextLink string `json:"nextLink"`
		}
		ans, err := p.az.send(apiARM, http.MethodGet, next, nil, nil)
		if err != nil {
			if isStatus(err, http.StatusForbidden) {
				return nil, refusef("Refused: the signed-in account cannot list the secrets of Key Vault %s. It needs Reader (or more) on the vault; its owner gives it in the Azure portal (the vault, Access control (IAM), Add role assignment).", vault.Name)
			}
			return nil, err
		}
		if json.Unmarshal(ans.body, &out) != nil {
			return nil, fmt.Errorf("Azure management's list of the secrets in Key Vault %s could not be read", vault.Name)
		}
		for _, s := range out.Value {
			name := s.Name
			if i := strings.LastIndex(name, "/"); i >= 0 {
				name = name[i+1:]
			}
			if name == "" {
				continue
			}
			if e := s.Properties.Attributes.Enabled; e != nil && !*e {
				name += " (disabled)"
			}
			names = append(names, name)
		}
		next = ""
		if u, err := url.Parse(out.NextLink); err == nil && u.Scheme == "https" && strings.EqualFold(u.Host, strings.TrimPrefix(armBase, "https://")) && strings.ToLower(u.Path) == under {
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
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	vault, err := p.findByName("Microsoft.KeyVault/vaults", args[0], "Key Vault")
	if err != nil {
		return "", err
	}
	names, err := p.secretNames(vault)
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return fmt.Sprintf("Key Vault %s holds no secrets.", args[0]), nil
	}
	return fmt.Sprintf("Secrets in Key Vault %s, by name only (%d):\n- %s", args[0], len(names), strings.Join(names, "\n- ")), nil
}
