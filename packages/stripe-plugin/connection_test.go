package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ---- the key is the stripe slot's API-key connection ----

func TestTheKeyIsReadFromTheStripeConnection(t *testing.T) {
	newFake(t)
	r := runPlugin(t, runOpts{}, "whoami")
	mustContain(t, r.text(), "account "+accountA, "environment test", "key rk_test_…")
	if strings.Contains(r.raw, keyA) {
		t.Fatal("the key was written out")
	}
}

// An update from 2.x carries the member's apiKey over into a connection whose field keeps that name.
func TestACarriedOverConnectionsApiKeyFieldIsRead(t *testing.T) {
	newFake(t)
	r := runPlugin(t, runOpts{keyField: "apiKey"}, "whoami")
	mustContain(t, r.text(), "account "+accountA, "environment test")
	if strings.Contains(r.raw, keyA) {
		t.Fatal("the key was written out")
	}
}

func TestWithNoStripeConnectionEveryCommandIsRefusedAndNothingIsSent(t *testing.T) {
	f := newFake(t)
	r := runPlugin(t, runOpts{noKey: true}, "setup")
	if r.ok() {
		t.Fatalf("setup ran with no key: %s", r.text())
	}
	mustContain(t, r.text(), "no Stripe key is bound", "Stripe preset", "Team settings -> Connections", "stripe slot")
	if len(f.calls) != 0 {
		t.Fatalf("the plugin called Stripe with no key: %v", f.calls)
	}
}

// A key handed under secrets - as a 2.x binding would - is not read: only the connection is.
func TestAKeyUnderSecretsIsNotRead(t *testing.T) {
	f := newFake(t)
	req := map[string]any{
		"protocol":         "harness.member/1",
		"work":             []any{map[string]any{"seq": 1, "instruction": "whoami"}},
		"config":           map[string]any{},
		"secrets":          map[string]any{"apiKey": keyA},
		"connections":      map[string]any{},
		"workingDirectory": t.TempDir(),
	}
	b, _ := json.Marshal(req)
	var out bytes.Buffer
	if code := run(bytes.NewReader(b), &out, func() time.Time { return testNow }); code == 0 {
		t.Fatalf("whoami ran with the key under secrets:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "no Stripe key is bound") || len(f.calls) != 0 {
		t.Fatalf("answer = %s, calls = %v", out.String(), f.calls)
	}
}
