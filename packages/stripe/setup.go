package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// permission is one permission the restricted key needs, and a request that proves it without
// changing anything: a write is proved by updating an object that does not exist, which Stripe
// answers 404 when the key may write and 403 when it may not.
type permission struct {
	resource, level string
	method, path    string
}

const probeID = "stripe_plugin_permission_check"

var permissions = []permission{
	{"Products", "write", http.MethodPost, "/v1/products/" + probeID},
	{"Prices", "write", http.MethodPost, "/v1/prices/price_" + probeID},
	{"Webhook Endpoints", "write", http.MethodPost, "/v1/webhook_endpoints/we_" + probeID},
	{"Checkout Sessions", "read", http.MethodGet, "/v1/checkout/sessions"},
	{"PaymentIntents", "read", http.MethodGet, "/v1/payment_intents"},
	{"Charges", "read", http.MethodGet, "/v1/charges"},
	{"Customers", "read", http.MethodGet, "/v1/customers"},
	{"Events", "read", http.MethodGet, "/v1/events"},
	{"Balance", "read", http.MethodGet, "/v1/balance"},
}

// checkSecret is the secret setup writes to each listed vault to prove it can be written. Its value
// is a fixed phrase, not a secret.
const checkSecret = "stripe-plugin-check"

// setup checks each thing in order and says the next step in words.
func (p *plugin) setup() (string, error) {
	lines := []string{"Stripe setup", "- key: restricted (rk_), " + p.env + " mode"}
	var next []string

	if err := p.checkAccount(false); err != nil {
		return "", err
	}
	if p.cfg.ExpectedAccount != "" {
		lines = append(lines, "- account: "+p.account+", as expectedAccount says")
	} else {
		if err := p.writePin(p.account); err != nil {
			return "", fmt.Errorf("the account could not be kept in this member's workspace: %v", err)
		}
		lines = append(lines, "- account: "+p.account+", kept for this member; every run now refuses a key for another account. To make it permanent, a person sets expectedAccount (Advanced settings) to "+p.account+".")
	}

	var missing []string
	for _, perm := range permissions {
		form := url.Values{}
		if perm.method == http.MethodGet {
			form.Set("limit", "1")
		}
		err := p.api.do(perm.method, perm.path, form, "", nil)
		switch {
		case err == nil, isStatus(err, http.StatusNotFound), isStatus(err, http.StatusBadRequest):
		case isStatus(err, http.StatusForbidden):
			missing = append(missing, perm.resource+" ("+perm.level+")")
		default:
			return "", fmt.Errorf("checking the %s permission: %v", perm.resource, err)
		}
	}
	if len(missing) == 0 {
		lines = append(lines, fmt.Sprintf("- permissions: all %d present", len(permissions)))
	} else {
		lines = append(lines, "- permissions: missing "+strings.Join(missing, ", "))
		next = append(next, "In the Stripe Dashboard (Developers, API keys), edit this restricted key and set "+
			strings.Join(missing, ", ")+", then run setup again.")
	}

	switch {
	case len(p.cfg.KeyVaults) == 0:
		lines = append(lines, "- Key Vault: none listed in keyVaults, so webhook ensure cannot create an endpoint")
		next = append(next, "To create webhook endpoints, a person adds the Key Vault's name to keyVaults and binds a Microsoft connection to the microsoft slot, then runs setup again.")
	case p.vault == "":
		lines = append(lines, "- Key Vault: "+strings.Join(p.cfg.KeyVaults, ", ")+" listed, but no Microsoft connection is bound to the microsoft slot")
		next = append(next, "A person binds a Microsoft connection that may set secrets in "+strings.Join(p.cfg.KeyVaults, ", ")+" to the microsoft slot in this member's settings, then runs setup again.")
	default:
		for _, vault := range p.cfg.KeyVaults {
			if _, err := p.kv.setSecret(vault, checkSecret, "write check", "text/plain"); err != nil {
				lines = append(lines, "- Key Vault "+vault+": cannot be written: "+err.Error())
				next = append(next, "Fix Key Vault "+vault+" as the line above says, then run setup again.")
			} else {
				lines = append(lines, "- Key Vault "+vault+": can be written (setup wrote the check secret "+checkSecret+")")
			}
		}
	}

	if p.applying() {
		lines = append(lines, "- mode: apply; ensure and rotate make changes")
	} else {
		lines = append(lines, "- mode: dryrun; every command says what it would do and changes nothing")
		if len(missing) == 0 {
			next = append(next, "When the dry runs say what you want, a person sets mode to apply.")
		}
	}

	if len(next) == 0 {
		lines = append(lines, "Ready: product ensure, price ensure and webhook ensure make changes now.")
	} else {
		lines = append(lines, "Next: "+next[0])
		for _, n := range next[1:] {
			lines = append(lines, "Then: "+n)
		}
	}
	return strings.Join(lines, "\n"), nil
}
