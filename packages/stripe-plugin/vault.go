package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// The Azure Resource Manager (Azure management) API versions the plugin speaks: listing
// subscriptions, listing the Key Vaults in one, and a vault's secrets.
const (
	subscriptionsAPIVersion = "2022-12-01"
	resourcesAPIVersion     = "2021-04-01"
	secretsAPIVersion       = "2023-07-01"
)

// armAPI is the address the Host keys Azure management's access token by in a connection's tokens.
const armAPI = "https://management.azure.com/"

// vaultClient writes a vault's secrets through Azure management, never through a vault's own data
// plane: the Microsoft sign-in asks for Azure management only. keyVaults names a vault by its name;
// the client finds the vault's resource id once per run.
type vaultClient struct {
	token string

	scanned bool
	scanErr error
	ids     map[string]string // vault name, lower case -> its resource id
}

// vaultResource matches the resource id of one Key Vault, as Azure management answers it.
var vaultResource = regexp.MustCompile(`(?i)^/subscriptions/[^/?#]+/resourceGroups/[^/?#]+/providers/Microsoft\.KeyVault/vaults/[^/?#]+$`)

const vaultFilter = "resourceType eq 'Microsoft.KeyVault/vaults'"

// errSignInRefused is Azure management's 401, the same for every request.
var errSignInRefused = errors.New("Azure management refused the Microsoft sign-in (401); reconnect the microsoft connection from Admin, Connections")

// maxPages bounds every list the plugin follows nextLink through.
const maxPages = 50

// idOf answers the resource id of the vault keyVaults names, looking through every subscription the
// signed-in account can read. The look-up is made once per run.
func (v *vaultClient) idOf(vault string) (string, error) {
	if v.token == "" {
		return "", refusal("Refused: no Microsoft connection is bound to the microsoft slot, so nothing can be written to Key Vault. A person binds one in this member's settings.")
	}
	if !v.scanned {
		v.scanned = true
		v.ids, v.scanErr = v.scan()
	}
	if v.scanErr != nil {
		return "", v.scanErr
	}
	if id, ok := v.ids[strings.ToLower(vault)]; ok {
		return id, nil
	}
	return "", fmt.Errorf("the signed-in Microsoft account cannot see a Key Vault named %s in any subscription it can read; give that account the Contributor or Key Vault Contributor role on the vault, or a person corrects the vault's name in keyVaults", vault)
}

type subscription struct {
	SubscriptionID string `json:"subscriptionId"`
	State          string `json:"state"`
}

type resource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// scan lists every Key Vault in every subscription the account can read.
func (v *vaultClient) scan() (map[string]string, error) {
	var subs []subscription
	err := v.list(armBase+"/subscriptions?api-version="+subscriptionsAPIVersion, "subscriptions", func(raw json.RawMessage) {
		var page []subscription
		_ = json.Unmarshal(raw, &page)
		subs = append(subs, page...)
	})
	if err != nil {
		return nil, err
	}

	ids := map[string]string{}
	query := "$filter=" + strings.ReplaceAll(url.QueryEscape(vaultFilter), "+", "%20") + "&api-version=" + resourcesAPIVersion
	for _, s := range subs {
		if s.SubscriptionID == "" || strings.EqualFold(s.State, "Disabled") || strings.EqualFold(s.State, "Deleted") {
			continue
		}
		target := armBase + "/subscriptions/" + url.PathEscape(s.SubscriptionID) + "/resources?" + query
		err := v.list(target, "a subscription's Key Vaults", func(raw json.RawMessage) {
			var page []resource
			_ = json.Unmarshal(raw, &page)
			for _, r := range page {
				if r.Name != "" && vaultResource.MatchString(r.ID) {
					ids[strings.ToLower(r.Name)] = r.ID
				}
			}
		})
		// A refused sign-in stops the scan; a subscription whose resources the account cannot list
		// holds no vault it can write, so it is passed over.
		if errors.Is(err, errSignInRefused) {
			return nil, err
		}
	}
	return ids, nil
}

// list reads every page of a list, following nextLink only on Azure management itself.
func (v *vaultClient) list(target, what string, each func(json.RawMessage)) error {
	for page := 0; page < maxPages && target != ""; page++ {
		status, raw, err := v.send(http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		if status/100 != 2 {
			return fmt.Errorf("Azure management answered %d to a list of %s", status, what)
		}
		var answer struct {
			Value    json.RawMessage `json:"value"`
			NextLink string          `json:"nextLink"`
		}
		if json.Unmarshal(raw, &answer) != nil {
			return fmt.Errorf("Azure management's list of %s could not be read", what)
		}
		each(answer.Value)
		target = ""
		if answer.NextLink != "" && onARM(answer.NextLink) {
			target = answer.NextLink
		}
	}
	return nil
}

// onARM reports whether an address is on Azure management itself, so the token is never sent to a
// nextLink anywhere else.
func onARM(raw string) bool {
	u, err := url.Parse(raw)
	base, berr := url.Parse(armBase)
	return err == nil && berr == nil && u.User == nil &&
		strings.EqualFold(u.Scheme, base.Scheme) && strings.EqualFold(u.Host, base.Host)
}

func secretURL(vaultID, name string) string {
	return armBase + vaultID + "/secrets/" + url.PathEscape(name) + "?api-version=" + secretsAPIVersion
}

var versionInURI = regexp.MustCompile(`/secrets/[^/]+/([^/?]+)$`)

// setSecret writes value as a new version of the secret and answers that version. The value goes
// only into the request body; the answer is read for the version and nothing else.
func (v *vaultClient) setSecret(vault, name, value, contentType string) (string, error) {
	id, err := v.idOf(vault)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{
		"properties": map[string]any{"value": value, "contentType": contentType},
		"tags":       map[string]string{"writtenBy": "stripe-plugin"},
	})
	status, raw, err := v.send(http.MethodPut, secretURL(id, name), body)
	if err != nil {
		return "", err
	}
	switch {
	case status == http.StatusForbidden:
		return "", fmt.Errorf("Key Vault %s does not let the signed-in account write secrets through Azure management (403); give that account the Contributor or Key Vault Contributor role on the vault (writing through Azure management needs no Key Vault data role)", vault)
	case status == http.StatusNotFound:
		return "", fmt.Errorf("Key Vault %s was not found (404); check its name in keyVaults", vault)
	case status/100 != 2:
		return "", fmt.Errorf("Key Vault %s answered %d", vault, status)
	}

	var answer struct {
		Properties struct {
			SecretURIWithVersion string `json:"secretUriWithVersion"`
		} `json:"properties"`
	}
	if json.Unmarshal(raw, &answer) != nil {
		return "", fmt.Errorf("Key Vault %s's answer could not be read", vault)
	}
	m := versionInURI.FindStringSubmatch(answer.Properties.SecretURIWithVersion)
	if m == nil {
		return "", fmt.Errorf("Key Vault %s did not name the new version", vault)
	}
	return m[1], nil
}

// reach proves Azure management answers the Microsoft sign-in for the vault with a read of one
// secret's resource, changing nothing. The secret need not exist: a 404 still means the vault was
// reached. Azure management never answers a secret's value, and the answer is not read.
func (v *vaultClient) reach(vault, name string) error {
	id, err := v.idOf(vault)
	if err != nil {
		return err
	}
	status, _, err := v.send(http.MethodGet, secretURL(id, name), nil)
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusForbidden:
		return fmt.Errorf("Key Vault %s does not let the signed-in account read its secrets through Azure management (403); give that account the Contributor or Key Vault Contributor role on the vault", vault)
	case status == http.StatusNotFound, status/100 == 2:
		return nil
	default:
		return fmt.Errorf("Key Vault %s answered %d", vault, status)
	}
}

// send makes one request to Azure management and answers the status and body. A refused sign-in
// (401) is an error here, the same for every request.
func (v *vaultClient) send(method, target string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("the request to Azure management could not be made")
	}
	req.Header.Set("Authorization", "Bearer "+v.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("Azure management could not be reached; try again later")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return 0, nil, errSignInRefused
	}
	return resp.StatusCode, raw, nil
}
