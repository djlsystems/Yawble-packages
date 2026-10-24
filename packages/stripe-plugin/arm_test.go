package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- Key Vault through Azure management ----

func TestTheWebhookSecretIsWrittenByAnARMPutToTheVaultsResourceIDAndNowhereElse(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)

	r := runPlugin(t, runOpts{workdir: dir, vault: true, config: apply(nil)},
		"webhook ensure "+hookURL+" checkout.session.completed "+allowedKV+"/stripe-webhook-secret")

	if !r.ok() {
		t.Fatalf("webhook ensure failed: %s", r.text())
	}
	secret := f.webhookSecs[0]
	writes := f.vaultWrites()
	if len(writes) != 1 {
		t.Fatalf("Azure management writes = %v", writes)
	}
	put := writes[0]
	if put.Method != http.MethodPut || put.Path != allowedVaultID+"/secrets/stripe-webhook-secret" || put.Query != "api-version=2023-07-01" {
		t.Fatalf("the secret was written by %s %s?%s", put.Method, put.Path, put.Query)
	}
	var body struct {
		Properties struct {
			Value       string `json:"value"`
			ContentType string `json:"contentType"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(put.Body), &body); err != nil || body.Properties.Value != secret || body.Properties.ContentType == "" {
		t.Fatalf("the PUT's body = %s", put.Body)
	}
	mustContain(t, r.text(), "Signing secret: Key Vault "+allowedKV+", secret stripe-webhook-secret, version v"+strings.Repeat("0", 30)+"1.")

	// The value is in that one request body and nowhere else: no other call's address or body, no
	// Stripe request, no output line of any kind (result, progress, publish), no workspace file.
	for _, c := range f.vaultCalls {
		if strings.Contains(c.Path+"?"+c.Query, secret) || (c.Method != http.MethodPut && strings.Contains(c.Body, secret)) {
			t.Fatalf("the secret reached Azure management outside the PUT's body: %+v", c)
		}
	}
	for _, c := range f.calls {
		if strings.Contains(c.Path, secret) || strings.Contains(c.Form.Encode(), secret) {
			t.Fatalf("the secret was sent to Stripe: %+v", c)
		}
	}
	for _, needle := range []string{secret, strings.TrimPrefix(secret, "whsec_"), "[redacted]"} {
		if strings.Contains(r.raw, needle) {
			t.Fatalf("output holds %q:\n%s", needle, r.raw)
		}
	}
	for _, p := range r.progress {
		if strings.Contains(p, secret) {
			t.Fatalf("progress holds the secret: %q", p)
		}
	}
	for _, p := range r.publish {
		if raw, _ := json.Marshal(p); strings.Contains(string(raw), secret) {
			t.Fatalf("an event holds the secret: %s", raw)
		}
	}
	if files := filesHolding(t, dir, secret); len(files) != 0 {
		t.Fatalf("workspace files hold the secret: %v", files)
	}
	if strings.Contains(r.raw, armToken) {
		t.Fatal("the Azure management token was written out")
	}
}

func TestTheVaultIsFoundOnALaterPageOfSubscriptionsAndANextLinkOffAzureManagementIsNotFollowed(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)

	r := runPlugin(t, runOpts{workdir: dir, vault: true, config: apply(nil)},
		"webhook ensure "+hookURL+" charge.succeeded "+allowedKV+"/hook-secret",
		"webhook ensure https://shop.example.com/other charge.succeeded "+allowedKV+"/other-secret")

	if !r.ok() {
		t.Fatalf("webhook ensure failed: %s", r.text())
	}
	if f.foreignHits != 0 {
		t.Fatalf("a nextLink on another host was followed %d times", f.foreignHits)
	}
	var lists, filters int
	for _, c := range f.vaultCalls {
		switch {
		case c.Path == "/subscriptions":
			lists++
		case strings.HasSuffix(c.Path, "/resources"):
			filters++
			if !strings.Contains(c.Query, "$filter=resourceType%20eq%20%27Microsoft.KeyVault%2Fvaults%27") {
				t.Fatalf("the resources filter was sent as %q", c.Query)
			}
		}
	}
	// Both pages of subscriptions, then each subscription's vaults, once for the whole run.
	if lists != 2 || filters != 2 {
		t.Fatalf("%d subscription list calls and %d resource lists, want 2 and 2 for one run", lists, filters)
	}
	if len(f.secrets[allowedKV+"/hook-secret"]) != 1 || len(f.secrets[allowedKV+"/other-secret"]) != 1 {
		t.Fatalf("secrets = %v", f.secrets)
	}
}

func TestAVaultInNoSubscriptionIsRefusedWithTheSentenceAndNoEndpointIsMade(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	cfg := apply(map[string]any{"keyVaults": []string{"missing-vault"}})

	r := runPlugin(t, runOpts{workdir: dir, vault: true, config: cfg},
		"webhook ensure "+hookURL+" charge.succeeded missing-vault/hook-secret")

	if r.ok() {
		t.Fatalf("result = %s", r.text())
	}
	mustContain(t, r.text(),
		"the signed-in Microsoft account cannot see a Key Vault named missing-vault in any subscription it can read",
		"Contributor or Key Vault Contributor role on the vault",
		"corrects the vault's name in keyVaults",
		"Nothing was changed")
	if len(f.writes()) != 0 || len(f.endpoints) != 0 || len(f.vaultWrites()) != 0 {
		t.Fatalf("something was changed: %v %v", f.writes(), f.vaultWrites())
	}

	s := runPlugin(t, runOpts{workdir: dir, vault: true, config: cfg}, "setup")
	mustContain(t, s.text(), "- Key Vault missing-vault: cannot be written: the signed-in Microsoft account cannot see a Key Vault named missing-vault")
}

func TestARefusedWriteNamesTheRoleTheAccountNeeds(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	runPlugin(t, runOpts{workdir: dir, vault: true, config: apply(nil)}, "webhook ensure "+hookURL+" charge.succeeded "+allowedKV+"/hook-secret")
	f.vaultStatus[allowedKV] = http.StatusForbidden

	r := runPlugin(t, runOpts{workdir: dir, vault: true, config: apply(nil)}, "webhook rotate "+hookURL+" "+allowedKV+"/hook-secret")

	if r.ok() {
		t.Fatalf("result = %s", r.text())
	}
	mustContain(t, r.text(), "Key Vault "+allowedKV+" does not let the signed-in account write secrets through Azure management (403)",
		"give that account the Contributor or Key Vault Contributor role on the vault",
		"writing through Azure management needs no Key Vault data role")
	if len(f.endpoints) != 1 {
		t.Fatalf("endpoints after a refused rotate = %v", f.endpoints)
	}
}

func TestAzureManagementsTokenIsTakenFromTokensBeforeAccessToken(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)

	r := runPlugin(t, runOpts{workdir: dir, vault: true, tokens: true, config: apply(nil)},
		"webhook ensure "+hookURL+" charge.succeeded "+allowedKV+"/hook-secret")

	if !r.ok() {
		t.Fatalf("webhook ensure with tokens failed: %s", r.text())
	}
	if len(f.secrets[allowedKV+"/hook-secret"]) != 1 {
		t.Fatalf("secrets = %v", f.secrets)
	}
	for _, tok := range []string{armToken, graphToken} {
		if strings.Contains(r.raw, tok) {
			t.Fatalf("a token was written out:\n%s", r.raw)
		}
	}
}

func TestNoCodeCallsAKeyVaultsOwnAddress(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, "vault.azure.net") {
				continue
			}
			// keyVaults may name a vault by its address: settings.go reads that form, and its refusal
			// says it. Nothing else names the address, so nothing sends a request to it.
			if name == "settings.go" && (strings.Contains(line, "TrimSuffix") || strings.Contains(line, "Refused:")) {
				continue
			}
			t.Fatalf("%s:%d names a Key Vault's own address: %s", name, i+1, strings.TrimSpace(line))
		}
		if strings.Contains(string(raw), "vaultBase") || strings.Contains(string(raw), "vaultAPIVersion") {
			t.Fatalf("%s still speaks to a vault's data plane", name)
		}
	}
}
