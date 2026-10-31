package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func apply(extra map[string]any) map[string]any {
	cfg := map[string]any{"mode": "apply", "keyVaults": []string{allowedKV}}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func mustContain(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(text, p) {
			t.Fatalf("answer lacks %q:\n%s", p, text)
		}
	}
}

// ---- setup ----

func TestSetupRefusesASecretKeyAndSendsNothing(t *testing.T) {
	f := newFake(t)
	r := runPlugin(t, runOpts{key: secretKey}, "setup")

	if r.ok() {
		t.Fatalf("an sk_ key was accepted: %s", r.text())
	}
	mustContain(t, r.text(), "secret key (sk_)", "restricted key (rk_)")
	if len(f.calls) != 0 {
		t.Fatalf("the plugin called Stripe with a secret key: %v", f.calls)
	}
	ev := r.events("refused")
	if len(ev) != 1 || ev[0]["command"] != "setup" || !strings.Contains(ev[0]["reason"].(string), "sk_") {
		t.Fatalf("refused event = %v", ev)
	}
	if strings.Contains(r.raw, secretKey) {
		t.Fatal("the secret key was written out")
	}
}

func TestSetupRefusesAKeyThatIsNotRestricted(t *testing.T) {
	newFake(t)
	r := runPlugin(t, runOpts{key: makeKey("pk", "test", "p")}, "setup")
	if r.ok() || !strings.Contains(r.text(), "rk_test_ or rk_live_") {
		t.Fatalf("result = %s", r.text())
	}
}

func TestSetupNamesEachMissingPermission(t *testing.T) {
	f := newFake(t)
	f.keys[keyA] = fakeKey{account: accountA, missing: map[string]bool{"Prices": true, "Events": true}}

	r := runPlugin(t, runOpts{}, "setup")

	if !r.ok() {
		t.Fatalf("setup failed: %s", r.text())
	}
	mustContain(t, r.text(),
		"- key: restricted (rk_), test mode",
		"- permissions: missing Prices (write), Events (read)",
		"Next: In the Stripe Dashboard (Developers, API keys), edit this restricted key and set Prices (write), Events (read), then run setup again.")
	if strings.Contains(r.text(), "Products (write)") || strings.Contains(r.text(), "Balance") {
		t.Fatalf("a present permission was named missing:\n%s", r.text())
	}
	if sent := f.sent(); len(sent) != 0 {
		t.Fatalf("setup in dryrun sent a write: %v", sent)
	}
	ev := r.events("refused")
	if len(ev) != 1 || ev[0]["command"] != "setup" || ev[0]["reason"] != "the restricted key lacks Prices (write), Events (read)" {
		t.Fatalf("refused event = %v", ev)
	}
	if strings.Contains(r.raw, keyA) {
		t.Fatal("the key was written out")
	}
}

func TestSetupInApplyNamesAMissingWritePermission(t *testing.T) {
	f := newFake(t)
	f.keys[keyA] = fakeKey{account: accountA, missing: map[string]bool{"Webhook Endpoints:write": true}}

	r := runPlugin(t, runOpts{config: map[string]any{"mode": "apply"}}, "setup")

	mustContain(t, r.text(), "- permissions: missing Webhook Endpoints (write)")
	if ev := r.events("refused"); len(ev) != 1 || ev[0]["reason"] != "the restricted key lacks Webhook Endpoints (write)" {
		t.Fatalf("refused event = %v", ev)
	}
}

func TestSetupInDryRunWritesNothingAnywhere(t *testing.T) {
	f := newFake(t)

	r := runPlugin(t, runOpts{vault: true, config: map[string]any{"keyVaults": []string{allowedKV}}}, "setup")

	if !r.ok() {
		t.Fatalf("setup failed: %s", r.text())
	}
	mustContain(t, r.text(),
		"- Key Vault "+allowedKV+": reached through Azure management with the Microsoft connection; dryrun writes nothing, so setup confirms the vault can be written when it runs with mode apply",
		"setup confirms Products, Prices and Webhook Endpoints (write) when it runs with mode apply")
	if sent := f.sent(); len(sent) != 0 {
		t.Fatalf("setup in dryrun sent a write to Stripe or Key Vault: %v", sent)
	}
	if len(f.vaultCalls) == 0 || len(f.vaultWrites()) != 0 {
		t.Fatalf("Azure management calls = %v", f.vaultCalls)
	}
	last := f.vaultCalls[len(f.vaultCalls)-1]
	if last.Path != allowedVaultID+"/secrets/"+checkSecret || last.Query != "api-version=2023-07-01" {
		t.Fatalf("setup in dryrun read %s?%s, want the check secret's resource", last.Path, last.Query)
	}
	if len(f.secrets) != 0 {
		t.Fatalf("setup in dryrun left secrets in the vault: %v", f.secrets)
	}
}

func TestSetupInDryRunSaysAVaultWithNoAccess(t *testing.T) {
	f := newFake(t)
	f.vaultStatus[allowedKV] = http.StatusForbidden

	r := runPlugin(t, runOpts{vault: true, config: map[string]any{"keyVaults": []string{allowedKV}}}, "setup")

	mustContain(t, r.text(), "- Key Vault "+allowedKV+": cannot be reached", "(403)", "Next: Fix Key Vault "+allowedKV)
	if sent := f.sent(); len(sent) != 0 {
		t.Fatalf("setup in dryrun sent a write: %v", sent)
	}
}

func TestSetupInApplyWritesTheCheckSecret(t *testing.T) {
	f := newFake(t)

	r := runPlugin(t, runOpts{vault: true, config: apply(nil)}, "setup")

	mustContain(t, r.text(), "- Key Vault "+allowedKV+": can be written (setup wrote the check secret "+checkSecret+")")
	if got := f.secrets[allowedKV+"/"+checkSecret]; len(got) != 1 || got[0] != "write check" {
		t.Fatalf("check secret = %v", got)
	}
	probes := 0
	for _, c := range f.calls {
		if c.Method == http.MethodPost && strings.Contains(c.Path, probeID) {
			probes++
		}
	}
	if probes != 3 {
		t.Fatalf("setup in apply sent %d write-permission probes, want 3", probes)
	}
}

func TestSetupKeepsTheAccountChecksTheVaultAndSaysTheNextStep(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()

	r := runPlugin(t, runOpts{workdir: dir, vault: true, config: map[string]any{"keyVaults": []string{"https://" + allowedKV + ".vault.azure.net/"}}}, "setup")

	if !r.ok() {
		t.Fatalf("setup failed: %s", r.text())
	}
	mustContain(t, r.text(),
		"- account: "+accountA+", kept for this member",
		"expectedAccount (Advanced settings) to "+accountA,
		"- permissions: all 9 present",
		"- Key Vault "+allowedKV+": reached through Azure management with the Microsoft connection",
		"- mode: dryrun",
		"Next: When the dry runs say what you want, a person sets mode to apply and runs setup again to confirm the writes.")
	if len(f.secrets) != 0 {
		t.Fatalf("setup in dryrun wrote to the vault: %v", f.secrets)
	}
	if pin := (&plugin{workdir: dir}).readPin(); pin != accountA {
		t.Fatalf("kept account = %q", pin)
	}
}

func TestSetupSaysAVaultThatCannotBeWritten(t *testing.T) {
	f := newFake(t)
	f.vaultStatus[allowedKV] = http.StatusForbidden

	r := runPlugin(t, runOpts{vault: true, config: apply(nil)}, "setup")

	mustContain(t, r.text(), "- Key Vault "+allowedKV+": cannot be written", "(403)", "Contributor or Key Vault Contributor role on the vault", "Next: Fix Key Vault "+allowedKV)
	if strings.Contains(r.text(), "Secrets Officer") || strings.Contains(r.text(), "access policy") {
		t.Fatalf("setup names a Key Vault data role:\n%s", r.text())
	}
}

func TestSetupSaysToBindTheConnectionWhenAVaultIsListed(t *testing.T) {
	newFake(t)
	r := runPlugin(t, runOpts{config: apply(nil)}, "setup")
	mustContain(t, r.text(), "no Microsoft connection is bound to the microsoft slot", "Next: A person binds a Microsoft connection")
}

func TestSetupInApplyWithEverythingInPlaceIsReady(t *testing.T) {
	newFake(t)
	r := runPlugin(t, runOpts{vault: true, config: apply(map[string]any{"expectedAccount": accountA})}, "setup")
	mustContain(t, r.text(), "- account: "+accountA+", as expectedAccount says", "Ready:")
}

func TestALiveKeyIsSaidToBeLive(t *testing.T) {
	newFake(t)
	r := runPlugin(t, runOpts{key: keyLive}, "whoami")
	mustContain(t, r.text(), "account "+accountA, "environment live", "key rk_live_…")
	if strings.Contains(r.raw, keyLive) {
		t.Fatal("the key was written out")
	}
}

// ---- the expected account ----

func TestAKeyForAnotherAccountThanExpectedAccountIsRefused(t *testing.T) {
	f := newFake(t)
	r := runPlugin(t, runOpts{key: keyB, config: apply(map[string]any{"expectedAccount": accountA})},
		"product ensure pro Pro")

	if r.ok() {
		t.Fatalf("a key for another account was accepted: %s", r.text())
	}
	mustContain(t, r.text(), "belongs to Stripe account "+accountB+", not "+accountA+" (expectedAccount)", "Nothing was changed")
	if len(f.writes()) != 0 || len(f.products) != 0 {
		t.Fatalf("something was changed: %v", f.writes())
	}
	ev := r.events("refused")
	if len(ev) != 1 || ev[0]["command"] != "product ensure" {
		t.Fatalf("refused event = %v", ev)
	}
}

func TestAKeySwappedAfterSetupIsRefused(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)

	for _, cmd := range []string{"whoami", "setup", "price ensure pro-once pro 100 usd", "events"} {
		r := runPlugin(t, runOpts{key: keyB, workdir: dir, config: apply(nil)}, cmd)
		if r.ok() || !strings.Contains(r.text(), "not "+accountA+" (the account setup kept for this member)") {
			t.Fatalf("%s with a swapped key: %s", cmd, r.text())
		}
	}
	if len(f.writes()) != 0 {
		t.Fatalf("something was changed: %v", f.writes())
	}
}

func TestCommandsWaitForSetup(t *testing.T) {
	f := newFake(t)
	r := runPlugin(t, runOpts{config: apply(nil)}, "product ensure pro Pro")
	if r.ok() || !strings.Contains(r.text(), "Run setup first") {
		t.Fatalf("result = %s", r.text())
	}
	if len(f.writes()) != 0 {
		t.Fatalf("something was changed: %v", f.writes())
	}
}

// ---- products and prices ----

func TestPriceEnsureTwiceMakesOnePrice(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	o := runOpts{workdir: dir, config: apply(nil)}

	if r := runPlugin(t, o, `product ensure pro "Pro plan" "Everything in one plan"`); !r.ok() {
		t.Fatalf("product ensure: %s", r.text())
	}
	first := runPlugin(t, o, "price ensure pro-once pro 1500 usd")
	second := runPlugin(t, o, "price ensure pro-once pro 1500 USD")

	if !first.ok() || !second.ok() {
		t.Fatalf("price ensure failed: %s / %s", first.text(), second.text())
	}
	if len(f.prices) != 1 {
		t.Fatalf("%d prices were made", len(f.prices))
	}
	mustContain(t, first.text(), "Created price pro-once")
	mustContain(t, second.text(), "is already there", "nothing to change")
	if len(second.events("changed")) != 0 {
		t.Fatalf("the second run published a change: %v", second.publish)
	}
	ev := first.events("changed")
	if len(ev) != 1 || ev[0]["objectType"] != "price" || ev[0]["lookupKey"] != "pro-once" || ev[0]["environment"] != "test" || ev[0]["action"] != "created" {
		t.Fatalf("changed event = %v", ev)
	}
}

func TestTheSameCreateSentTwiceCarriesOneIdempotencyKey(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	o := runOpts{workdir: dir, config: apply(nil)}
	runPlugin(t, o, "product ensure pro Pro")
	runPlugin(t, o, "price ensure pro-once pro 1500 usd")
	// Stripe has not listed the first price yet: the retry must reuse the same idempotency key.
	f.prices[0]["lookup_key"] = "hidden-for-the-test"
	runPlugin(t, o, "price ensure pro-once pro 1500 usd")

	var keys []string
	for _, c := range f.writes() {
		if c.Path == "/v1/prices" {
			keys = append(keys, c.Idem)
		}
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] || len(f.prices) != 1 {
		t.Fatalf("idempotency keys %v, %d prices", keys, len(f.prices))
	}
}

func TestProductEnsureTwiceMakesOneProductAndUpdatesTheName(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	o := runOpts{workdir: dir, config: apply(nil)}

	runPlugin(t, o, "product ensure pro Pro")
	same := runPlugin(t, o, "product ensure pro Pro")
	renamed := runPlugin(t, o, `product ensure pro "Pro plan"`)

	if len(f.products) != 1 {
		t.Fatalf("%d products", len(f.products))
	}
	mustContain(t, same.text(), "already there")
	mustContain(t, renamed.text(), "Updated product pro")
	if f.products["pro"]["name"] != "Pro plan" {
		t.Fatalf("product = %v", f.products["pro"])
	}
}

func TestANewAmountMovesTheLookupKeyToANewPrice(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	o := runOpts{workdir: dir, config: apply(nil)}
	runPlugin(t, o, "product ensure pro Pro")
	runPlugin(t, o, "price ensure pro-once pro 1500 usd")

	r := runPlugin(t, o, "price ensure pro-once pro 1900 usd")

	mustContain(t, r.text(), "moved lookup key pro-once", "is archived")
	if len(f.prices) != 2 || f.prices[0]["active"] != false || f.prices[1]["lookup_key"] != "pro-once" {
		t.Fatalf("prices = %v", f.prices)
	}
}

func TestAPriceForAMissingProductIsRefusedInApply(t *testing.T) {
	newFake(t)
	dir := setUp(t, keyA)
	r := runPlugin(t, runOpts{workdir: dir, config: apply(nil)}, "price ensure pro-once nothing 1500 usd")
	if r.ok() || !strings.Contains(r.text(), "no product nothing") {
		t.Fatalf("result = %s", r.text())
	}
}

func TestBadArgumentsAreRefusedNamingTheShape(t *testing.T) {
	newFake(t)
	dir := setUp(t, keyA)
	for cmd, want := range map[string]string{
		"price ensure pro-once pro 15.00 usd":    "whole number",
		"price ensure pro-once pro 1500 dollars": "three-letter",
		"price ensure pro-once pro":              "<amount in cents>",
		"product ensure bad/key Pro":             "lookup key",
		"webhook ensure http://shop.example.com/hook charge.succeeded " + allowedKV + "/s": "https://",
		"payment show ch_123": "starting pi_",
		"frobnicate":          "Unknown command",
	} {
		r := runPlugin(t, runOpts{workdir: dir, config: apply(nil), vault: true}, cmd)
		if r.ok() || !strings.Contains(r.text(), want) {
			t.Fatalf("%s: %s", cmd, r.text())
		}
	}
}

// ---- dry run ----

func TestDryRunMakesNothing(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	o := runOpts{workdir: dir, vault: true, config: map[string]any{"keyVaults": []string{allowedKV}}}

	r := runPlugin(t, o,
		`product ensure pro "Pro plan"`,
		"price ensure pro-once pro 1500 usd",
		"webhook ensure https://shop.example.com/stripe/webhook checkout.session.completed "+allowedKV+"/stripe-webhook-secret")

	if !r.ok() {
		t.Fatalf("dry run failed: %s", r.text())
	}
	mustContain(t, r.text(), "1. Would create product pro", "2. Would create price pro-once", "3. Would create a webhook endpoint", "Dry run: nothing was changed")
	if len(f.writes()) != 0 {
		t.Fatalf("dry run changed Stripe: %v", f.writes())
	}
	if len(f.vaultCalls) != 0 {
		t.Fatalf("dry run wrote to Key Vault: %v", f.vaultCalls)
	}
	if len(f.products)+len(f.prices)+len(f.endpoints) != 0 || len(r.events("changed")) != 0 {
		t.Fatal("dry run made something")
	}
}

func TestDryRunOnExistingObjectsSaysWhatWouldChange(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	runPlugin(t, runOpts{workdir: dir, vault: true, config: apply(nil)},
		"product ensure pro Pro",
		"price ensure pro-once pro 1500 usd",
		"webhook ensure https://shop.example.com/hook charge.succeeded "+allowedKV+"/hook-secret")
	before := len(f.writes())

	r := runPlugin(t, runOpts{workdir: dir, vault: true, config: map[string]any{"keyVaults": []string{allowedKV}}},
		`product ensure pro "Pro plan"`,
		"price ensure pro-once pro 1900 usd",
		"webhook ensure https://shop.example.com/hook charge.succeeded charge.refunded "+allowedKV+"/hook-secret",
		"webhook rotate https://shop.example.com/hook "+allowedKV+"/hook-secret")

	mustContain(t, r.text(), "Would update product pro", "Would create a new price pro-once", "Would update webhook endpoint", "Would create a new webhook endpoint")
	if len(f.writes()) != before {
		t.Fatalf("dry run changed Stripe: %v", f.writes()[before:])
	}
}

// ---- webhooks and Key Vault ----

const hookURL = "https://shop.example.com/stripe/webhook"

func TestWebhookEnsureWritesTheSigningSecretToKeyVaultAndNowhereElse(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	o := runOpts{workdir: dir, vault: true, config: apply(nil)}

	r := runPlugin(t, o, "webhook ensure "+hookURL+" checkout.session.completed payment_intent.payment_failed "+allowedKV+"/stripe-webhook-secret")

	if !r.ok() {
		t.Fatalf("webhook ensure failed: %s", r.text())
	}
	if len(f.webhookSecs) != 1 || len(f.endpoints) != 1 {
		t.Fatalf("%d secrets, %d endpoints", len(f.webhookSecs), len(f.endpoints))
	}
	secret := f.webhookSecs[0]
	if got := f.secrets[allowedKV+"/stripe-webhook-secret"]; len(got) != 1 || got[0] != secret {
		t.Fatalf("Key Vault holds %v, want the signing secret", got)
	}
	mustContain(t, r.text(), "Signing secret: Key Vault "+allowedKV+", secret stripe-webhook-secret, version v")

	// The scan: no output line of any kind - result, progress row, published event - and no file in
	// the workspace holds the secret, whole or in part, nor was it redacted out of one.
	for _, needle := range []string{secret, strings.TrimPrefix(secret, "whsec_"), "whsec_", "[redacted]"} {
		if strings.Contains(r.raw, needle) {
			t.Fatalf("output holds %q:\n%s", needle, r.raw)
		}
	}
	for _, line := range r.lines {
		if line["t"] != "result" && line["t"] != "progress" && line["t"] != "publish" {
			t.Fatalf("unexpected record %v", line)
		}
	}
	if len(r.progress) == 0 || len(r.events("changed")) != 1 {
		t.Fatalf("progress %v, events %v", r.progress, r.publish)
	}
	if files := filesHolding(t, dir, secret, strings.TrimPrefix(secret, "whsec_")); len(files) != 0 {
		t.Fatalf("workspace files hold the secret: %v", files)
	}
	ev := r.events("changed")[0]
	if ev["objectType"] != "webhook_endpoint" || ev["lookupKey"] != hookURL || ev["action"] != "created" {
		t.Fatalf("changed event = %v", ev)
	}

	// Ensured again: the endpoint is there, nothing new is written anywhere.
	again := runPlugin(t, o, "webhook ensure "+hookURL+" payment_intent.payment_failed checkout.session.completed "+allowedKV+"/stripe-webhook-secret")
	mustContain(t, again.text(), "already there")
	if len(f.endpoints) != 1 || len(f.secrets[allowedKV+"/stripe-webhook-secret"]) != 1 {
		t.Fatal("ensuring again made another endpoint or secret version")
	}

	// New events update the endpoint in place.
	updated := runPlugin(t, o, "webhook ensure "+hookURL+" checkout.session.completed "+allowedKV+"/stripe-webhook-secret")
	mustContain(t, updated.text(), "Updated webhook endpoint")
	if len(f.endpoints) != 1 || len(f.endpoints[0]["enabled_events"].([]string)) != 1 {
		t.Fatalf("endpoints = %v", f.endpoints)
	}
}

func TestWebhookRotateMakesANewEndpointAndSecretThenRemovesTheOld(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	o := runOpts{workdir: dir, vault: true, config: apply(nil)}
	runPlugin(t, o, "webhook ensure "+hookURL+" checkout.session.completed "+allowedKV+"/hook-secret")
	oldID := f.endpoints[0]["id"]

	r := runPlugin(t, o, "webhook rotate "+hookURL+" "+allowedKV+"/hook-secret")

	if !r.ok() {
		t.Fatalf("rotate failed: %s", r.text())
	}
	if len(f.endpoints) != 1 || f.endpoints[0]["id"] == oldID {
		t.Fatalf("endpoints after rotate = %v", f.endpoints)
	}
	versions := f.secrets[allowedKV+"/hook-secret"]
	if len(versions) != 2 || versions[1] != f.webhookSecs[1] || versions[0] == versions[1] {
		t.Fatalf("Key Vault versions = %d", len(versions))
	}
	mustContain(t, r.text(), "version v", fmt.Sprintf("Removed the old endpoint %s.", oldID))
	for _, s := range f.webhookSecs {
		if strings.Contains(r.raw, s) || len(filesHolding(t, dir, s)) != 0 {
			t.Fatal("a signing secret was written out")
		}
	}
	// Created comes before deleted, so nothing is ever without an endpoint.
	var order []string
	for _, c := range f.writes() {
		if strings.HasPrefix(c.Path, "/v1/webhook_endpoints") {
			order = append(order, c.Method)
		}
	}
	if strings.Join(order, " ") != "POST POST DELETE" {
		t.Fatalf("webhook calls = %v", order)
	}
}

func TestAFailedVaultWriteRemovesTheNewEndpoint(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	f.vaultStatus[allowedKV] = http.StatusForbidden

	r := runPlugin(t, runOpts{workdir: dir, vault: true, config: apply(nil)}, "webhook ensure "+hookURL+" charge.succeeded "+allowedKV+"/hook-secret")

	if r.ok() {
		t.Fatalf("result = %s", r.text())
	}
	mustContain(t, r.text(), "(403)", "Contributor or Key Vault Contributor role on the vault", "needs no Key Vault data role", "removed again")
	if len(f.endpoints) != 0 || strings.Contains(r.raw, f.webhookSecs[0]) {
		t.Fatalf("endpoints left: %v", f.endpoints)
	}
}

func TestAVaultNotListedIsRefused(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)

	for _, cmd := range []string{
		"webhook ensure " + hookURL + " charge.succeeded " + otherKV + "/hook-secret",
		"webhook ensure " + hookURL + " charge.succeeded https://" + otherKV + ".vault.azure.net/hook-secret",
		"webhook rotate " + hookURL + " " + otherKV + "/hook-secret",
	} {
		r := runPlugin(t, runOpts{workdir: dir, vault: true, config: apply(nil)}, cmd)
		if r.ok() {
			t.Fatalf("%s was accepted: %s", cmd, r.text())
		}
		mustContain(t, r.text(), "Key Vault "+otherKV+" is not in keyVaults (keyVaults lists "+allowedKV+")")
		ev := r.events("refused")
		if len(ev) != 1 || !strings.HasPrefix(ev[0]["command"].(string), "webhook ") {
			t.Fatalf("refused event = %v", ev)
		}
	}
	if len(f.writes()) != 0 || len(f.vaultCalls) != 0 {
		t.Fatalf("something was changed: %v %v", f.writes(), f.vaultCalls)
	}
}

func TestWebhookEnsureWithoutTheConnectionIsRefused(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	r := runPlugin(t, runOpts{workdir: dir, config: apply(nil)}, "webhook ensure "+hookURL+" charge.succeeded "+allowedKV+"/hook-secret")
	if r.ok() || !strings.Contains(r.text(), "microsoft slot") {
		t.Fatalf("result = %s", r.text())
	}
	if len(f.writes()) != 0 {
		t.Fatalf("something was changed: %v", f.writes())
	}
}

// ---- read-only commands ----

func TestPaymentShowNamesTheCardByBrandAndLastFourOnly(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	f.payment = map[string]any{
		"id": "pi_123", "status": "succeeded", "amount": 1500, "amount_received": 1500, "currency": "usd",
		"customer": "cus_1", "created": testNow.Unix(),
		"latest_charge": map[string]any{
			"id": "ch_1",
			"billing_details": map[string]any{"name": "Pat Example", "email": "pat@example.com",
				"address": map[string]any{"line1": "1 Example Street", "postal_code": "00000"}},
			"payment_method_details": map[string]any{"card": map[string]any{
				"brand": "visa", "last4": "4242", "exp_month": 12, "exp_year": 2031,
				"fingerprint": "FakeFingerprint99", "funding": "credit", "country": "US",
				"network_token": map[string]any{"used": false}, "iin": "424242",
			}},
		},
	}

	r := runPlugin(t, runOpts{workdir: dir}, "payment show pi_123")

	mustContain(t, r.text(), "payment pi_123", "status succeeded", "card visa ending 4242")
	for _, never := range []string{"2031", "FakeFingerprint99", "424242", "credit", "Example Street", "pat@example.com", "Pat Example", "exp"} {
		if strings.Contains(r.raw, never) {
			t.Fatalf("output holds %q:\n%s", never, r.text())
		}
	}
	for _, c := range f.calls {
		if c.Path == "/v1/payment_intents/pi_123" && c.Form.Get("expand[]") != "latest_charge" {
			t.Fatalf("payment read = %v", c.Form)
		}
	}
}

func TestReadsAnswerAtMostFiftyItems(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	for i := 0; i < 70; i++ {
		f.events = append(f.events, map[string]any{"id": fmt.Sprintf("evt_%03d", i), "type": "charge.succeeded",
			"created": testNow.Unix() - int64(i), "data": map[string]any{"object": map[string]any{"id": "ch_x", "object": "charge",
				"payment_method_details": map[string]any{"card": map[string]any{"exp_year": 2031, "fingerprint": "FakeFingerprint99"}}}}})
	}

	r := runPlugin(t, runOpts{workdir: dir}, "events charge.succeeded 7d")

	if !r.ok() {
		t.Fatalf("events failed: %s", r.text())
	}
	if n := strings.Count(r.text(), "evt_"); n != 50 {
		t.Fatalf("%d events shown", n)
	}
	mustContain(t, r.text(), "At most 50 are shown")
	if strings.Contains(r.raw, "FakeFingerprint99") || strings.Contains(r.raw, "2031") {
		t.Fatal("event payload details were written out")
	}
	var asked bool
	for _, c := range f.calls {
		if c.Path == "/v1/events" {
			asked = c.Form.Get("limit") == "50" && c.Form.Get("type") == "charge.succeeded" &&
				c.Form.Get("created[gte]") == fmt.Sprint(testNow.Add(-7*24*3600e9).Unix())
		}
	}
	if !asked {
		t.Fatal("events did not ask Stripe for at most 50 of that type since then")
	}
}

func TestCustomerFindAndCheckoutShowAskForTheirOwnObjects(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	r := runPlugin(t, runOpts{workdir: dir}, "customer find pat@example.com", "checkout show cs_test_123")

	mustContain(t, r.text(), "1. No customer has the email pat@example.com", "2. there is no Checkout Session cs_test_123")
	var customers bool
	for _, c := range f.calls {
		if c.Path == "/v1/customers" && c.Form.Get("email") == "pat@example.com" && c.Form.Get("limit") == "50" {
			customers = true
		}
	}
	if !customers {
		t.Fatal("customer find did not ask for at most 50 by email")
	}
}

func TestStripesOwnMessagesNeverCarryTheKeyOut(t *testing.T) {
	newFake(t)
	r := runPlugin(t, runOpts{key: makeKey("rk", "test", "e")}, "whoami")
	if r.ok() || strings.Contains(r.raw, "eeee") {
		t.Fatalf("result = %s", r.raw)
	}
	mustContain(t, r.text(), "Stripe does not accept the key in the stripe connection (401)")
}
