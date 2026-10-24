package main

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The Monitor draws plugin.stripe-plugin.changed: one event per object made, updated or removed in Stripe,
// in apply mode only, and nothing secret in it.

type change struct{ objectType, id, lookupKey, action string }

func changes(r records) []change {
	var out []change
	for _, ev := range r.events("changed") {
		out = append(out, change{ev["objectType"].(string), ev["id"].(string), ev["lookupKey"].(string), ev["action"].(string)})
	}
	return out
}

// mustHoldNoSecret fails when any published event carries a key, a token, a signing secret, a
// webhook URL's query, or a field outside the five declared ones.
func mustHoldNoSecret(t *testing.T, f *fake, r records, query string) {
	t.Helper()
	needles := append([]string{keyA, armToken, "whsec_", "[redacted]", query}, f.webhookSecs...)
	for _, p := range r.publish {
		line := strings.ToLower(stringify(p))
		for _, n := range needles {
			if n != "" && strings.Contains(line, strings.ToLower(n)) {
				t.Fatalf("a published event holds %q: %v", n, p)
			}
		}
		if p["type"] != "changed" {
			continue
		}
		var keys []string
		for k := range p["payload"].(map[string]any) {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if want := []string{"action", "environment", "id", "lookupKey", "objectType"}; !slices.Equal(keys, want) {
			t.Fatalf("changed carries %v, want %v", keys, want)
		}
		if p["payload"].(map[string]any)["environment"] != "test" {
			t.Fatalf("changed environment = %v", p["payload"])
		}
	}
}

func stringify(v any) string {
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				b.WriteString(k + " ")
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		case string:
			b.WriteString(x + " ")
		}
	}
	walk(v)
	return b.String()
}

func TestEveryChangeInApplyPublishesOneChangedPerObjectWithNoSecret(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	o := runOpts{workdir: dir, vault: true, config: apply(nil)}
	// A webhook URL may carry a token in its query; it is never published.
	query := "token=" + strings.Repeat("q", 24)
	hook := hookURL + "?" + query
	target := allowedKV + "/hook-secret"

	steps := []struct {
		cmd  string
		want func() []change
	}{
		{`product ensure pro "Pro plan"`, func() []change { return []change{{"product", "pro", "pro", "created"}} }},
		{`product ensure pro "Pro plan two"`, func() []change { return []change{{"product", "pro", "pro", "updated"}} }},
		{"price ensure pro-once pro 1500 usd", func() []change {
			return []change{{"price", f.prices[0]["id"].(string), "pro-once", "created"}}
		}},
		// A new amount makes a new price and archives the old one: two objects, two events.
		{"price ensure pro-once pro 2000 usd", func() []change {
			return []change{{"price", f.prices[1]["id"].(string), "pro-once", "created"}, {"price", f.prices[0]["id"].(string), "pro-once", "updated"}}
		}},
		{"webhook ensure " + hook + " charge.succeeded " + target, func() []change {
			return []change{{"webhook_endpoint", f.endpoints[0]["id"].(string), hookURL, "created"}}
		}},
		{"webhook ensure " + hook + " charge.succeeded charge.refunded " + target, func() []change {
			return []change{{"webhook_endpoint", f.endpoints[0]["id"].(string), hookURL, "updated"}}
		}},
	}
	for _, s := range steps {
		r := runPlugin(t, o, s.cmd)
		if !r.ok() {
			t.Fatalf("%s failed: %s", s.cmd, r.text())
		}
		if got, want := changes(r), s.want(); !slices.Equal(got, want) {
			t.Fatalf("%s published %v, want %v", s.cmd, got, want)
		}
		mustHoldNoSecret(t, f, r, query)
	}

	// Rotate makes one endpoint and removes one: one event each, created before deleted.
	oldID := f.endpoints[0]["id"].(string)
	r := runPlugin(t, o, "webhook rotate "+hook+" "+target)
	if !r.ok() {
		t.Fatalf("rotate failed: %s", r.text())
	}
	want := []change{{"webhook_endpoint", f.endpoints[0]["id"].(string), hookURL, "created"}, {"webhook_endpoint", oldID, hookURL, "deleted"}}
	if got := changes(r); !slices.Equal(got, want) {
		t.Fatalf("rotate published %v, want %v", got, want)
	}
	mustHoldNoSecret(t, f, r, query)

	// Nothing to change, a read, a check or a refusal publishes no changed.
	for _, cmd := range []string{
		`product ensure pro "Pro plan two"`,
		"price ensure pro-once pro 2000 usd",
		"webhook ensure " + hook + " charge.succeeded charge.refunded " + target,
		"setup", "whoami", "customer find a@example.com", "events",
		"webhook rotate " + hook + " " + otherKV + "/hook-secret",
	} {
		if r := runPlugin(t, o, cmd); len(r.events("changed")) != 0 {
			t.Fatalf("%s published %v", cmd, r.publish)
		}
	}
}

func TestDryRunPublishesNoChanged(t *testing.T) {
	f := newFake(t)
	dir := setUp(t, keyA)
	applied := runOpts{workdir: dir, vault: true, config: apply(nil)}
	target := allowedKV + "/hook-secret"
	runPlugin(t, applied, `product ensure pro "Pro plan"`, "price ensure pro-once pro 1500 usd", "webhook ensure "+hookURL+" charge.succeeded "+target)
	before := len(f.writes())

	dry := runOpts{workdir: dir, vault: true, config: map[string]any{"mode": "dryrun", "keyVaults": []string{allowedKV}}}
	for _, cmd := range []string{
		`product ensure pro "Another name"`,
		`product ensure basic "Basic plan"`,
		"price ensure pro-once pro 999 usd",
		"webhook ensure " + hookURL + " charge.refunded " + target,
		"webhook ensure https://shop.example.com/other charge.refunded " + target,
		"webhook rotate " + hookURL + " " + target,
		"setup",
	} {
		r := runPlugin(t, dry, cmd)
		if !r.ok() {
			t.Fatalf("%s failed: %s", cmd, r.text())
		}
		if len(r.events("changed")) != 0 {
			t.Fatalf("dry run %s published %v", cmd, r.publish)
		}
	}
	if len(f.writes()) != before {
		t.Fatalf("dry run changed Stripe: %v", f.writes()[before:])
	}
}

// An endpoint is made before its secret goes to Key Vault. When the vault write fails it is removed
// again and nothing is published; when removing it fails too, it stays in Stripe and is published.
func TestAnEndpointLeftInStripeByAFailedRollbackIsPublished(t *testing.T) {
	cases := []struct {
		name                   string
		vaultFails, noSecret   bool
		deleteFails, published bool
	}{
		{"vault write fails, removed again", true, false, false, false},
		{"vault write fails, removal fails", true, false, true, true},
		{"no signing secret, removed again", false, true, false, false},
		{"no signing secret, removal fails", false, true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake(t)
			dir := setUp(t, keyA)
			if c.vaultFails {
				f.vaultStatus[allowedKV] = http.StatusForbidden
			}
			f.noSecret, f.deleteFails = c.noSecret, c.deleteFails

			r := runPlugin(t, runOpts{workdir: dir, vault: true, config: apply(nil)}, "webhook ensure "+hookURL+" charge.succeeded "+allowedKV+"/hook-secret")

			if r.ok() {
				t.Fatalf("result = %s", r.text())
			}
			var want []change
			if c.published {
				if len(f.endpoints) != 1 {
					t.Fatalf("endpoints = %v", f.endpoints)
				}
				want = []change{{"webhook_endpoint", f.endpoints[0]["id"].(string), hookURL, "created"}}
				mustContain(t, r.text(), "delete it in the Stripe Dashboard")
			} else if len(f.endpoints) != 0 {
				t.Fatalf("endpoints left: %v", f.endpoints)
			}
			if got := changes(r); !slices.Equal(got, want) {
				t.Fatalf("published %v, want %v", got, want)
			}
			mustHoldNoSecret(t, f, r, "")
			for _, s := range f.webhookSecs {
				if strings.Contains(r.raw, s) || len(filesHolding(t, dir, s)) != 0 {
					t.Fatal("a signing secret was written out")
				}
			}
		})
	}
}
