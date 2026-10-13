// STRIPE: a plugin member, protocol harness.member/1, that sets up Stripe products, one-time prices
// and webhook endpoints through a restricted key, and reads checkouts, payments, customers and
// events. A webhook signing secret goes from Stripe's answer straight into an Azure Key Vault,
// written through Azure Resource Manager, and is never written anywhere else.
//
// In:  ONE JSON request on stdin - { work: [{ instruction }], config, secrets: { apiKey },
//
//	connections: { microsoft: { accessToken, tokens } }, workingDirectory, ... }.
//
// Out: JSON Lines on stdout - progress, publish, then exactly one result.
//
// One command per instruction; the first words are the command (commands.go lists them). Changes
// happen only in mode=apply; dryrun looks everything up and says what it would do.
//
// Self-contained: standard library only, built with CGO_ENABLED=0 (build.sh). The Stripe and Azure
// management base URLs are package variables only tests change; the shipped binary has no way to
// point elsewhere.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

type request struct {
	Work             []workItem                 `json:"work"`
	Config           map[string]json.RawMessage `json:"config"`
	Secrets          map[string]string          `json:"secrets"`
	Connections      map[string]connection      `json:"connections"`
	WorkingDirectory string                     `json:"workingDirectory"`
}

type workItem struct {
	Instruction *string         `json:"instruction"`
	Payload     json.RawMessage `json:"payload"`
}

// connection is a bound slot on stdin. accessToken is the first API's token; tokens, when present,
// holds a token per API, keyed by the API's address (https://management.azure.com/).
type connection struct {
	Provider    string              `json:"provider"`
	AccessToken string              `json:"accessToken"`
	Tokens      map[string]apiToken `json:"tokens"`
}

type apiToken struct {
	AccessToken string `json:"accessToken"`
}

// managementToken answers the token for Azure management: tokens' entry for it when there is one,
// else accessToken.
func (c connection) managementToken() string {
	for api, t := range c.Tokens {
		if strings.TrimSuffix(strings.ToLower(api), "/") == strings.TrimSuffix(armAPI, "/") && t.AccessToken != "" {
			return t.AccessToken
		}
	}
	return c.AccessToken
}

// slot is the manifest's one connection slot, for Azure management only.
const slot = "microsoft"

// refusal is a sentence the plugin refuses a command with, word for word. A refusal publishes
// plugin.stripe.refused; any other error does not.
type refusal string

func (r refusal) Error() string { return string(r) }

type plugin struct {
	out     io.Writer
	cfg     settings
	key     string
	env     string // test or live, from the key
	vault   string // the Microsoft access token for Azure management, "" when the slot is unbound
	workdir string
	api     *stripeClient
	kv      *vaultClient
	now     func() time.Time

	// account is the Stripe account the key belongs to, once a command has checked it this run.
	account string
	// hidden holds every value that must never be written out: the key, the vault token and each
	// signing secret this run handled.
	hidden []string
}

func main() { os.Exit(run(os.Stdin, os.Stdout, time.Now)) }

func run(in io.Reader, out io.Writer, now func() time.Time) int {
	p := &plugin{out: out, now: now}

	var req request
	raw, err := io.ReadAll(in)
	if err == nil {
		err = json.Unmarshal(raw, &req)
	}
	if err != nil {
		p.fail("The request on stdin could not be read.")
		return 1
	}

	cfg, problem := parseSettings(req.Config)
	if problem != "" {
		p.fail(problem)
		return 1
	}
	p.cfg = cfg
	p.key = strings.TrimSpace(req.Secrets["apiKey"])
	p.hide(p.key)
	if c, ok := req.Connections[slot]; ok {
		p.vault = c.managementToken()
		p.hide(c.AccessToken)
		for _, t := range c.Tokens {
			p.hide(t.AccessToken)
		}
	}
	p.workdir = req.WorkingDirectory
	p.api = &stripeClient{key: p.key}
	p.kv = &vaultClient{token: p.vault}

	if len(req.Work) == 0 {
		p.fail(unknownCommand(""))
		return 1
	}

	// THE KEY IS CHECKED BEFORE ANY COMMAND: a secret key never reaches Stripe from this plugin.
	env, why := keyKind(p.key)
	if why != "" {
		p.refuse(commandName(instructionText(req.Work[0])), why)
		p.fail(why)
		return 1
	}
	p.env = env

	var texts, errs []string
	for i, item := range req.Work {
		text := instructionText(item)
		p.progress(fmt.Sprintf("stripe: %s, mode %s, %s key", commandName(text), p.cfg.Mode, p.env))
		answer, err := p.dispatch(text)
		prefix := ""
		if len(req.Work) > 1 {
			prefix = fmt.Sprintf("%d. ", i+1)
		}
		if err != nil {
			if r, ok := err.(refusal); ok {
				p.refuse(commandName(text), string(r))
			}
			errs = append(errs, prefix+err.Error())
			texts = append(texts, prefix+err.Error())
			continue
		}
		texts = append(texts, prefix+answer)
	}

	if len(errs) > 0 {
		if len(req.Work) > 1 {
			p.fail(strings.Join(texts, "\n\n"))
		} else {
			p.fail(errs[0])
		}
		return 1
	}
	p.emit(map[string]any{"t": "result", "ok": true, "output": strings.Join(texts, "\n\n")})
	return 0
}

// keyKind says whether the key is test or live, or why it is refused.
func keyKind(key string) (env, why string) {
	switch {
	case key == "":
		return "", "Refused: no Stripe key is set. A person binds the apiKey secret to a restricted key (rk_...) in this member's settings."
	case strings.HasPrefix(key, "sk_"):
		return "", "Refused: apiKey is a secret key (sk_), which can do anything in the account. Make a restricted key (rk_) with the permissions the README lists, bind it instead, and roll the secret key if it was shared. Nothing was sent to Stripe."
	case strings.HasPrefix(key, "rk_test_"):
		return "test", ""
	case strings.HasPrefix(key, "rk_live_"):
		return "live", ""
	default:
		return "", "Refused: apiKey is not a Stripe restricted key; it must start rk_test_ or rk_live_. Nothing was sent to Stripe."
	}
}

func instructionText(item workItem) string {
	if item.Instruction != nil {
		return *item.Instruction
	}
	var payload struct {
		Instruction string `json:"instruction"`
	}
	if len(item.Payload) > 0 && json.Unmarshal(item.Payload, &payload) == nil {
		return payload.Instruction
	}
	return ""
}

func (p *plugin) hide(value string) {
	if len(value) >= 4 {
		p.hidden = append(p.hidden, value)
	}
}

// keyShaped matches anything shaped like a Stripe key or signing secret, masked or not, so a value
// echoed in one of Stripe's own messages is never passed on.
var keyShaped = regexp.MustCompile(`\b(?:sk|rk|pk)_(?:test|live)_[A-Za-z0-9*]+|\bwhsec_[A-Za-z0-9*]+`)

// emit writes one record. Every line goes through scrub first: no key, token or signing secret
// leaves the plugin, whatever text carried it.
func (p *plugin) emit(record any) {
	line, _ := json.Marshal(record)
	_, _ = p.out.Write(append([]byte(p.scrub(string(line))), '\n'))
}

func (p *plugin) scrub(text string) string {
	for _, v := range p.hidden {
		text = strings.ReplaceAll(text, v, "[redacted]")
	}
	return keyShaped.ReplaceAllString(text, "[redacted]")
}

func (p *plugin) progress(status string) {
	p.emit(map[string]any{"t": "progress", "status": status})
}

func (p *plugin) fail(sentence string) {
	p.emit(map[string]any{"t": "result", "ok": false, "error": sentence})
}

func (p *plugin) publish(suffix string, payload map[string]any) {
	p.emit(map[string]any{"t": "publish", "type": suffix, "payload": payload})
}

func (p *plugin) refuse(command, reason string) {
	p.publish("refused", map[string]any{"command": command, "reason": reason})
}

// changed publishes plugin.stripe.changed for one object made, updated or removed in apply mode.
func (p *plugin) changed(objectType, id, lookupKey, action string) {
	p.publish("changed", map[string]any{
		"objectType": objectType, "id": id, "lookupKey": lookupKey,
		"environment": p.env, "action": action,
	})
}
