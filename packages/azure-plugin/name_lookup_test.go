package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// From CON 15 (Yawble 2af730c) the Host sends each bound connection's name just before its account.
// secret=<ENV>=<connection>.<field> finds the connection by that name, ignoring case, and never by
// account: two API keys with the preset Other are both account "Other". Every fixture's name differs
// from its account, so a lookup by account would fail these tests.

// The fixtures are shaped exactly as the Host writes stdin. testdata/host-request-masked.json is
// Lumi's masked capture of a real Host's request (B0080 evidence), with each connection's name put
// just before its account as PluginMemberRunner.ConnectionNode does at 2af730c, and one website
// added in ConnectionNode's credential shape (kind, name, account, preset, fields), which the capture
// did not hold. The keys of every object - the request, a work item, its payload, each connection
// kind - come in the same order in runPlugin's request.
func TestTheFixtureIsShapedLikeTheHostsRequest(t *testing.T) {
	raw, err := os.ReadFile("testdata/host-request-masked.json")
	if err != nil {
		t.Fatal(err)
	}
	ours, err := json.Marshal(hostRequest(t, runOpts{
		noConnect: true, // the capture's probe had no microsoft slot
		moved:     map[string]string{"appSecret1": "x"},
		bound:     []hostObject{mailbox("mail", "bot@example.test", "x"), login("FTP upload", "ftpuser", "x"), website("Customer portal", "x", "y")},
	}, "containerapp create rg1 app1 env1 reg12345.azurecr.io/app:1"))
	if err != nil {
		t.Fatal(err)
	}
	withMicrosoft, err := json.Marshal(hostRequest(t, runOpts{}, "x"))
	if err != nil {
		t.Fatal(err)
	}
	host, mine := keyOrders(t, raw), keyOrders(t, ours)
	for path, want := range host {
		if strings.HasPrefix(path, "config") || strings.Contains(path, ".fields") {
			continue // the member's settings and a credential's own fields are data, not shape
		}
		if got, ok := mine[path]; !ok || got != want {
			t.Errorf("%s: the Host sends keys %s, the fixture %s", path, want, got)
		}
	}
	// The microsoft slot is not in the capture: Grant at 2af730c writes provider, name, account,
	// accessToken, expiresAt, scopes, tokens.
	if got, want := keyOrders(t, withMicrosoft)["connections.microsoft"], "provider,name,account,accessToken,expiresAt,scopes,tokens"; got != want {
		t.Errorf("connections.microsoft: %s, want %s", got, want)
	}
	// The request the Host sends is what run reads: every connection with its name.
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, k := range keyConnections(req.Connections[keySlot]) {
		names = append(names, k.Name+"/"+k.Account)
	}
	if got := strings.Join(names, "; "); got != "mail/bot@example.test; FTP upload/<masked: equals login username ftpuser-LOGINU>; Stripe live/Stripe; Old app secrets/Other; Customer portal/portal.example.test" {
		t.Fatalf("names/accounts = %s", got)
	}
}

// keyOrders answers each object's keys in order, by path; an array's objects are keyed by their
// kind (connections) or their place, so each connection kind is compared with its own.
func keyOrders(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var walk func(path string) error
	walk = func(path string) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'):
			var keys []string
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return err
				}
				keys = append(keys, k.(string))
				if err := walk(strings.TrimPrefix(path+"."+k.(string), ".")); err != nil {
					return err
				}
			}
			out[path] = strings.Join(keys, ",")
			_, err = dec.Token()
			return err
		case json.Delim('['):
			for i := 0; dec.More(); i++ {
				if err := walk(fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	if err := walk(""); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	// Re-key appSecrets entries by kind.
	byKind := map[string]string{}
	for path, keys := range out {
		if !strings.HasPrefix(path, "connections.appSecrets[") {
			byKind[path] = keys
		}
	}
	var conns []map[string]any
	var doc struct {
		Connections map[string]json.RawMessage `json:"connections"`
	}
	if json.Unmarshal(raw, &doc) == nil && json.Unmarshal(doc.Connections[keySlot], &conns) == nil {
		for i, c := range conns {
			prefix := fmt.Sprintf("connections.appSecrets[%d]", i)
			kind := fmt.Sprintf("connections.appSecrets{%v}", c["kind"])
			for path, keys := range out {
				if path == prefix || strings.HasPrefix(path, prefix+".") {
					byKind[kind+strings.TrimPrefix(path, prefix)] = keys
				}
			}
		}
	}
	return byKind
}

func TestAWebsiteConnectionsFieldsGoIntoTheApp(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{config: apply(nil), bound: []hostObject{website("Customer portal", siteValue, tokenValue)}},
		appCreateLine(`"secret=PORTAL_PASSWORD=Customer portal.password" "secret=PORTAL_TOKEN=customer portal.token" "secret=PORTAL_URL=CUSTOMER PORTAL.siteUrl" "secret=PORTAL_USER=Customer portal.username"`))
	if !r.ok() {
		t.Fatal(r.text())
	}
	assertNoNamedValue(t, r.raw)
	secrets, env := appPut(t, w)
	mustInline(t, secrets, env, "PORTAL_PASSWORD", "portal-password", siteValue)
	mustInline(t, secrets, env, "PORTAL_TOKEN", "portal-token", tokenValue)
	mustInline(t, secrets, env, "PORTAL_URL", "portal-url", "https://portal.example.test/")
	mustInline(t, secrets, env, "PORTAL_USER", "portal-user", "portal-bot")
	onlyTheAppCarries(t, w, siteValue, tokenValue)

	// Its fields, in a refusal, are named; none of its values is.
	newWorld(t)
	r = runPlugin(t, runOpts{config: apply(nil), bound: []hostObject{website("Customer portal", siteValue, tokenValue)}},
		appCreateLine(`"secret=PORTAL=Customer portal.apiKey"`))
	mustContain(t, r.text(), "the field apiKey of the connection Customer portal, which that connection does not have or holds no value in; Customer portal has the fields mayAct, method, password, siteUrl, token, tokenHeader, username,", "Nothing was changed.")
	assertNoNamedValue(t, r.raw)
}

func TestTheConnectionsNameMatchesIgnoringCase(t *testing.T) {
	w := newWorld(t)
	r := runPlugin(t, runOpts{config: apply(nil), bound: []hostObject{mailbox("Billing Mailbox", "billing@example.test", secretValue)}},
		appCreateLine(`"secret=SMTP_PASSWORD=billing MAILBOX.Password"`))
	if !r.ok() {
		t.Fatal(r.text())
	}
	assertNoNamedValue(t, r.raw)
	secrets, env := appPut(t, w)
	mustInline(t, secrets, env, "SMTP_PASSWORD", "smtp-password", secretValue)
	onlyTheAppCarries(t, w, secretValue)
}

// Two API keys with the preset Other are both account "Other"; each is found by its own name, and
// "Other" itself names neither.
func TestTwoOtherAPIKeysAreToldApartByName(t *testing.T) {
	o := runOpts{config: apply(nil), keys: map[string]map[string]string{"Shop webhooks": {"key": otherValue}, "Blog webhooks": {"key": keyValue}}}
	w := newWorld(t)
	r := runPlugin(t, o, appCreateLine(`"secret=SHOP_HOOK=shop webhooks.key" "secret=BLOG_HOOK=Blog Webhooks.KEY"`))
	if !r.ok() {
		t.Fatal(r.text())
	}
	assertNoNamedValue(t, r.raw)
	secrets, env := appPut(t, w)
	mustInline(t, secrets, env, "SHOP_HOOK", "shop-hook", otherValue)
	mustInline(t, secrets, env, "BLOG_HOOK", "blog-hook", keyValue)

	w = newWorld(t)
	r = runPlugin(t, o, appCreateLine("secret=HOOK=Other.key"))
	if r.ok() {
		t.Fatal("a connection was found by its account")
	}
	mustContain(t, r.text(), "Refused: the secret HOOK reads Other.key, the field key of the connection Other, which is not bound to this member's appSecrets slot (the ones bound are Blog webhooks, Shop webhooks)", "Nothing was changed.")
	if len(w.f.calls) != 0 {
		t.Fatalf("Azure was called %d times", len(w.f.calls))
	}
	assertNoNamedValue(t, r.raw)
}

// Each kind is found by its name and never by its account.
func TestAConnectionIsNeverFoundByItsAccount(t *testing.T) {
	bound := []hostObject{
		mailbox("mail", "bot@example.test", secretValue), login("Shop login", "shopbot", otherValue),
		apiKey("stripe live", "Stripe", "stripe", map[string]string{"key": keyValue}), website("Customer portal", siteValue, tokenValue),
	}
	for _, arg := range []string{"secret=A=bot@example.test.password", "secret=A=shopbot.password", "secret=A=Stripe.key", "secret=A=portal.example.test.password"} {
		w := newWorld(t)
		r := runPlugin(t, runOpts{config: apply(nil), bound: bound}, appCreateLine(arg))
		if r.ok() {
			t.Fatalf("%s was found by account", arg)
		}
		mustContain(t, r.text(), "which is not bound to this member's appSecrets slot (the ones bound are mail, Shop login, stripe live, Customer portal)", "Nothing was changed.")
		if len(w.f.calls) != 0 {
			t.Fatalf("Azure was called %d times", len(w.f.calls))
		}
		assertNoNamedValue(t, r.raw)
	}
}

// A Host before CON 15 sends no name: <connection>.<field> is refused saying the Host is too old,
// whichever account would have matched, and nothing is changed.
func TestAConnectionWithNoNameIsRefusedAsAHostTooOld(t *testing.T) {
	for _, bound := range [][]hostObject{
		{unnamed(mailbox("mail", "mail", secretValue))},
		{mailbox("mail", "bot@example.test", secretValue), unnamed(apiKey("stripe", "Stripe", "stripe", map[string]string{"key": keyValue}))},
	} {
		w := newWorld(t)
		r := runPlugin(t, runOpts{config: apply(nil), bound: bound}, appCreateLine("secret=SMTP_PASSWORD=mail.password"))
		if r.ok() {
			t.Fatal("a connection with no name was guessed")
		}
		mustContain(t, r.text(), "Refused: the secret SMTP_PASSWORD reads mail.password, the field password of the connection mail, but the Host did not send the name of every connection bound to this member's appSecrets slot: it is too old to send connection names",
			"ask the person to update Yawble", "Nothing was changed.")
		if len(w.f.calls) != 0 {
			t.Fatalf("Azure was called %d times", len(w.f.calls))
		}
		if ev := r.events("refused"); len(ev) != 1 {
			t.Fatalf("refused events = %v", ev)
		}
		assertNoNamedValue(t, r.raw)
	}
}

// The order the refusals list the bound connections is binding order, and each by its name.
func TestBoundNamesAreListedByName(t *testing.T) {
	p := &plugin{keys: keyConnections(json.RawMessage(mustJSON(t, []any{
		apiKey("Zeta", "Other", "other", map[string]string{"k": "v"}), unnamed(apiKey("x", "Other", "other", map[string]string{"k": "v"})),
	})))}
	if got := p.boundNames(); got != "Zeta, unnamed" {
		t.Fatalf("boundNames = %q", got)
	}
}
