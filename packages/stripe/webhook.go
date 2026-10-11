package main

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var (
	eventName  = regexp.MustCompile(`^(\*|[a-z_]+(\.[a-z_]+)*(\.\*)?)$`)
	secretName = regexp.MustCompile(`^[0-9A-Za-z-]{1,127}$`)
)

type endpoint struct {
	ID            string   `json:"id"`
	URL           string   `json:"url"`
	EnabledEvents []string `json:"enabled_events"`
	// Secret is in Stripe's answer to a create only. It is passed to Key Vault and nowhere else.
	Secret string `json:"secret"`
}

// vaultTarget is <vault>/<secret name>, the vault being one keyVaults lists.
type vaultTarget struct{ vault, name string }

func (t vaultTarget) String() string { return t.vault + "/" + t.name }

func (p *plugin) parseTarget(arg string) (vaultTarget, error) {
	a := strings.TrimPrefix(strings.TrimSpace(arg), "https://")
	i := strings.LastIndex(a, "/")
	if i <= 0 || i == len(a)-1 {
		return vaultTarget{}, refusal(fmt.Sprintf("Refused: %q is not <vault>/<secret name>, such as payments-vault/stripe-webhook-secret.", arg))
	}
	vault, ok := normaliseVault(a[:i])
	name := a[i+1:]
	if !ok {
		return vaultTarget{}, refusal(fmt.Sprintf("Refused: %q is not a Key Vault name (3 to 24 letters, digits and hyphens).", a[:i]))
	}
	if !secretName.MatchString(name) {
		return vaultTarget{}, refusal(fmt.Sprintf("Refused: %q is not a Key Vault secret name (1 to 127 letters, digits and hyphens).", name))
	}
	if !p.cfg.vaultAllowed(vault) {
		listed := "keyVaults is empty"
		if len(p.cfg.KeyVaults) > 0 {
			listed = "keyVaults lists " + strings.Join(p.cfg.KeyVaults, ", ")
		}
		return vaultTarget{}, refusal(fmt.Sprintf("Refused: Key Vault %s is not in keyVaults (%s); only a person adds a vault there. Nothing was changed.", vault, listed))
	}
	return vaultTarget{vault: vault, name: name}, nil
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return refusal(fmt.Sprintf("Refused: the webhook URL %q must be a public https:// address.", raw))
	}
	return nil
}

// endpointsFor answers every webhook endpoint for that URL, oldest first.
func (p *plugin) endpointsFor(target string) ([]endpoint, error) {
	var found []endpoint
	form := url.Values{"limit": {"100"}}
	for page := 0; page < 10; page++ {
		var list struct {
			Data    []endpoint `json:"data"`
			HasMore bool       `json:"has_more"`
		}
		if err := p.api.do(http.MethodGet, "/v1/webhook_endpoints", form, "", &list); err != nil {
			return nil, err
		}
		for _, e := range list.Data {
			if e.URL == target {
				found = append(found, e)
			}
		}
		if !list.HasMore || len(list.Data) == 0 {
			break
		}
		form.Set("starting_after", list.Data[len(list.Data)-1].ID)
	}
	// Stripe lists newest first.
	for i, j := 0, len(found)-1; i < j; i, j = i+1, j-1 {
		found[i], found[j] = found[j], found[i]
	}
	return found, nil
}

func sameEvents(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// createWithSecret makes an endpoint and writes its signing secret to the vault. When the write
// fails the endpoint is removed again, so no endpoint is left whose secret nobody has.
func (p *plugin) createWithSecret(target string, events []string, to vaultTarget) (endpoint, string, error) {
	form := url.Values{"url": {target}, "description": {"Managed by the stripe plugin; signing secret in Key Vault " + to.String()}}
	for _, e := range events {
		form.Add("enabled_events[]", e)
	}
	var made endpoint
	if err := p.api.do(http.MethodPost, "/v1/webhook_endpoints", form, "", &made); err != nil {
		return endpoint{}, "", err
	}
	secret := made.Secret
	made.Secret = ""
	p.hide(secret)
	if secret == "" {
		_ = p.api.do(http.MethodDelete, "/v1/webhook_endpoints/"+made.ID, nil, "", nil)
		return endpoint{}, "", fmt.Errorf("Stripe did not answer the new endpoint's signing secret, so the endpoint was removed again; nothing was written to Key Vault")
	}
	version, err := p.kv.setSecret(to.vault, to.name, secret, "Stripe webhook signing secret")
	if err != nil {
		if derr := p.api.do(http.MethodDelete, "/v1/webhook_endpoints/"+made.ID, nil, "", nil); derr != nil {
			return endpoint{}, "", fmt.Errorf("%v. Removing the new endpoint %s failed too (%v); delete it in the Stripe Dashboard", err, made.ID, derr)
		}
		if _, ok := err.(refusal); ok {
			return endpoint{}, "", err
		}
		return endpoint{}, "", fmt.Errorf("%v. The new endpoint was removed again, so nothing changed", err)
	}
	return made, version, nil
}

// webhook ensure <url> <event...> <vault>/<secret name>
func (p *plugin) webhookEnsure(args []string) (string, error) {
	if len(args) < 3 {
		return "", refusal("Refused: webhook ensure takes <url> <event...> <vault>/<secret name>, such as: webhook ensure https://shop.example.com/stripe/webhook checkout.session.completed payments-vault/stripe-webhook-secret")
	}
	target, events := args[0], args[1:len(args)-1]
	to, err := p.parseTarget(args[len(args)-1])
	if err != nil {
		return "", err
	}
	if err := checkURL(target); err != nil {
		return "", err
	}
	for _, e := range events {
		if !eventName.MatchString(e) {
			return "", refusal(fmt.Sprintf("Refused: %q is not a Stripe event type, such as checkout.session.completed or *.", e))
		}
	}

	found, err := p.endpointsFor(target)
	if err != nil {
		return "", err
	}

	if len(found) > 0 {
		ep := found[len(found)-1]
		if sameEvents(ep.EnabledEvents, events) {
			return fmt.Sprintf("Webhook endpoint %s for %s is already there with those events; nothing to change. Its signing secret was written to Key Vault when it was made; webhook rotate makes a new one.", ep.ID, target), nil
		}
		if !p.applying() {
			return fmt.Sprintf("Would update webhook endpoint %s for %s to the events %s. %s", ep.ID, target, strings.Join(events, ", "), dryRunNote), nil
		}
		form := url.Values{}
		for _, e := range events {
			form.Add("enabled_events[]", e)
		}
		if err := p.api.do(http.MethodPost, "/v1/webhook_endpoints/"+ep.ID, form, "", nil); err != nil {
			return "", err
		}
		p.changed("webhook_endpoint", ep.ID, target, "updated")
		return fmt.Sprintf("Updated webhook endpoint %s for %s to the events %s. Its signing secret is unchanged.", ep.ID, target, strings.Join(events, ", ")), nil
	}

	if p.vault == "" {
		return "", refusal("Refused: no Microsoft connection is bound to the microsoft slot, so the signing secret could not be written to Key Vault. A person binds one in this member's settings. Nothing was changed.")
	}
	if !p.applying() {
		return fmt.Sprintf("Would create a webhook endpoint for %s with the events %s and write its signing secret to Key Vault as %s. %s", target, strings.Join(events, ", "), to, dryRunNote), nil
	}
	made, version, err := p.createWithSecret(target, events, to)
	if err != nil {
		return "", err
	}
	p.changed("webhook_endpoint", made.ID, target, "created")
	return fmt.Sprintf("Created webhook endpoint %s for %s with the events %s.\nSigning secret: Key Vault %s, secret %s, version %s.", made.ID, target, strings.Join(events, ", "), to.vault, to.name, version), nil
}

// webhook rotate <url> <vault>/<secret name>
func (p *plugin) webhookRotate(args []string) (string, error) {
	if len(args) != 2 {
		return "", refusal("Refused: webhook rotate takes <url> <vault>/<secret name>.")
	}
	target := args[0]
	to, err := p.parseTarget(args[1])
	if err != nil {
		return "", err
	}
	if err := checkURL(target); err != nil {
		return "", err
	}
	found, err := p.endpointsFor(target)
	if err != nil {
		return "", err
	}
	if len(found) == 0 {
		return "", refusal(fmt.Sprintf("Refused: there is no webhook endpoint for %s to rotate; webhook ensure makes one.", target))
	}
	if p.vault == "" {
		return "", refusal("Refused: no Microsoft connection is bound to the microsoft slot, so a new signing secret could not be written to Key Vault. A person binds one in this member's settings. Nothing was changed.")
	}
	old := found[len(found)-1]
	var oldIDs []string
	for _, e := range found {
		oldIDs = append(oldIDs, e.ID)
	}
	if !p.applying() {
		return fmt.Sprintf("Would create a new webhook endpoint for %s with the events %s, write its signing secret to Key Vault as %s, then remove %s. %s", target, strings.Join(old.EnabledEvents, ", "), to, strings.Join(oldIDs, ", "), dryRunNote), nil
	}

	made, version, err := p.createWithSecret(target, old.EnabledEvents, to)
	if err != nil {
		return "", err
	}
	p.changed("webhook_endpoint", made.ID, target, "created")
	lines := []string{
		fmt.Sprintf("Created webhook endpoint %s for %s with the events %s.", made.ID, target, strings.Join(old.EnabledEvents, ", ")),
		fmt.Sprintf("Signing secret: Key Vault %s, secret %s, version %s.", to.vault, to.name, version),
	}
	for _, e := range found {
		if err := p.api.do(http.MethodDelete, "/v1/webhook_endpoints/"+e.ID, nil, "", nil); err != nil {
			lines = append(lines, fmt.Sprintf("Removing the old endpoint %s failed (%v); both send events until it is deleted in the Stripe Dashboard.", e.ID, err))
			continue
		}
		p.changed("webhook_endpoint", e.ID, target, "deleted")
		lines = append(lines, "Removed the old endpoint "+e.ID+".")
	}
	return strings.Join(lines, "\n"), nil
}
