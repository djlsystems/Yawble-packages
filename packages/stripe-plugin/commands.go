package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const commandList = "setup, whoami, product ensure, price ensure, webhook ensure, webhook rotate, checkout show, payment show, customer find, events"

func unknownCommand(text string) string {
	if strings.TrimSpace(text) == "" {
		return "No command was given. The commands are: " + commandList + "."
	}
	return fmt.Sprintf("Unknown command %q. The commands are: %s.", commandName(text), commandList)
}

// twoWord names the commands whose name is two words.
var twoWord = map[string]bool{"product": true, "price": true, "webhook": true, "checkout": true, "payment": true, "customer": true}

// commandName is the command without its arguments, for progress rows and plugin.stripe-plugin.refused.
func commandName(text string) string {
	words := strings.Fields(strings.ToLower(text))
	switch {
	case len(words) == 0:
		return ""
	case twoWord[words[0]] && len(words) > 1:
		return words[0] + " " + words[1]
	default:
		return words[0]
	}
}

func (p *plugin) dispatch(text string) (string, error) {
	args, err := splitArgs(text)
	if err != nil {
		return "", refusal("Refused: " + err.Error())
	}
	if len(args) == 0 {
		return "", refusal(unknownCommand(""))
	}
	name := commandName(text)
	rest := args[1:]
	if twoWord[strings.ToLower(args[0])] {
		if len(args) < 2 {
			return "", refusal(unknownCommand(text))
		}
		rest = args[2:]
	}

	switch name {
	case "setup":
		return p.setup()
	case "whoami":
		return p.whoami()
	}

	if err := p.checkAccount(true); err != nil {
		return "", err
	}
	switch name {
	case "product ensure":
		return p.productEnsure(rest)
	case "price ensure":
		return p.priceEnsure(rest)
	case "webhook ensure":
		return p.webhookEnsure(rest)
	case "webhook rotate":
		return p.webhookRotate(rest)
	case "checkout show":
		return p.checkoutShow(rest)
	case "payment show":
		return p.paymentShow(rest)
	case "customer find":
		return p.customerFind(rest)
	case "events":
		return p.events(rest)
	}
	return "", refusal(unknownCommand(text))
}

// splitArgs splits on spaces, keeping "double" or 'single' quoted words together.
func splitArgs(text string) ([]string, error) {
	var args []string
	var cur strings.Builder
	var quote rune
	inWord := false
	for _, r := range strings.TrimSpace(text) {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("a quote is not closed")
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args, nil
}

// pinFile keeps the account setup found, in the member's workspace. It holds the account id only.
const pinFile = "stripe-account.json"

func (p *plugin) pinPath() string {
	dir := p.workdir
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, pinFile)
}

func (p *plugin) readPin() string {
	raw, err := os.ReadFile(p.pinPath())
	if err != nil {
		return ""
	}
	var pin struct {
		Account string `json:"account"`
	}
	if json.Unmarshal(raw, &pin) != nil || !accountName.MatchString(pin.Account) {
		return ""
	}
	return pin.Account
}

func (p *plugin) writePin(account string) error {
	raw, _ := json.Marshal(map[string]string{"account": account})
	return os.WriteFile(p.pinPath(), append(raw, '\n'), 0o600)
}

// expected answers the account the key must belong to and where that comes from: the setting, or
// the account setup kept for this member.
func (p *plugin) expected() (string, string) {
	if p.cfg.ExpectedAccount != "" {
		return p.cfg.ExpectedAccount, "expectedAccount"
	}
	if pin := p.readPin(); pin != "" {
		return pin, "the account setup kept for this member"
	}
	return "", ""
}

// checkAccount refuses a key for another account than the expected one. With needSetup, a member
// that has no expected account yet is refused until setup has run.
func (p *plugin) checkAccount(needSetup bool) error {
	if p.account != "" {
		return nil
	}
	acct, err := p.api.accountID()
	if err != nil {
		return err
	}
	want, source := p.expected()
	switch {
	case want == "" && needSetup:
		return refusal("Refused: setup has not run for this key yet, so the account it belongs to is not known. Run setup first. Nothing was changed.")
	case want != "" && acct != want:
		return refusal(fmt.Sprintf("Refused: the stripe connection's key belongs to Stripe account %s, not %s (%s). Nothing was changed. If the key was changed to another account on purpose, a person sets expectedAccount to %s and runs setup.", acct, want, source, acct))
	}
	p.account = acct
	return nil
}

func (p *plugin) whoami() (string, error) {
	if err := p.checkAccount(false); err != nil {
		return "", err
	}
	lines := []string{
		"account " + p.account,
		"environment " + p.env,
		"key rk_" + p.env + "_… (restricted)",
	}
	if want, source := p.expected(); want != "" {
		lines = append(lines, "expected account "+want+" ("+source+")")
	} else {
		lines = append(lines, "expected account: not set yet; run setup")
	}
	return strings.Join(lines, "\n"), nil
}

func (p *plugin) applying() bool { return p.cfg.Mode == "apply" }

// dryRunNote ends every answer that would have changed something.
const dryRunNote = "Dry run: nothing was changed; a person sets mode to apply to make it."
