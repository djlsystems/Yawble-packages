package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const commandList = "setup, list <domain>, ensure <domain> <type> <host> <value> [ttl] [mxpref], remove <domain> <type> <host> [value], restore <domain> <backup id>"

func unknownCommand(word string) string {
	if word == "" {
		return "No command was given. The namecheap commands are: " + commandList + "."
	}
	return fmt.Sprintf("Unknown command %q. The namecheap commands are: %s.", word, commandList)
}

// split reads an instruction into words. A value with spaces goes in double or single quotes:
// ensure example.com TXT @ "v=spf1 include:_spf.example.net ~all".
func split(text string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	for _, r := range text {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, refusal("Refused: a quote is not closed. Put a value with spaces in one pair of double or single quotes. Nothing was changed.")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

func (p *plugin) dispatch(text string) (string, error) {
	words, err := split(strings.TrimSpace(text))
	if err != nil {
		p.refused("", "", err)
		return "", err
	}
	if len(words) > 0 && strings.EqualFold(words[0], "namecheap") {
		words = words[1:]
	}
	if len(words) == 0 {
		return "", refusal(unknownCommand(""))
	}
	cmd, args := strings.ToLower(words[0]), words[1:]

	var out string
	switch cmd {
	case "setup":
		return p.setup(), nil
	case "list":
		out, err = p.list(args)
	case "ensure":
		out, err = p.ensure(args)
	case "remove":
		out, err = p.remove(args)
	case "restore":
		out, err = p.restore(args)
	default:
		return "", refusal(unknownCommand(words[0]))
	}
	if err != nil {
		domain := ""
		if len(args) > 0 {
			domain = normaliseDomain(args[0])
		}
		p.refused(cmd, domain, err)
	}
	return out, err
}

// refused publishes plugin.namecheap.refused for a command that was turned down or failed.
func (p *plugin) refused(command, domain string, err error) {
	payload := map[string]any{"command": command, "reason": err.Error()}
	if domain != "" {
		payload["domain"] = domain
	}
	p.publish("refused", payload)
}

func (p *plugin) domainArg(args []string, usage string, min, max int) (string, error) {
	if len(args) < min || len(args) > max {
		return "", refusal("Usage: " + usage + ". Nothing was changed.")
	}
	return normaliseDomain(args[0]), nil
}

// ---- list ------------------------------------------------------------------------------------

func (p *plugin) list(args []string) (string, error) {
	domain, err := p.domainArg(args, "list <domain>", 1, 1)
	if err != nil {
		return "", err
	}
	set, err := p.readSet(domain)
	if err != nil {
		return "", err
	}
	s := fmt.Sprintf("%s: %d records on Namecheap DNS (mail setting %s, mode %s).\n%s", domain, len(set.Records), orNone(set.EmailType), p.cfg.Mode, lines(set.Records))
	if ids := p.recentBackups(domain, 5); len(ids) > 0 {
		s += "\nNewest backups: " + strings.Join(ids, ", ") + "."
	}
	return s, nil
}

// ---- ensure ----------------------------------------------------------------------------------

func (p *plugin) ensure(args []string) (string, error) {
	const usage = "ensure <domain> <type> <host> <value> [ttl] [mxpref], with a value holding spaces in quotes"
	domain, err := p.domainArg(args, usage, 4, 6)
	if err != nil {
		return "", err
	}
	typ, err := recordType(args[1], false)
	if err != nil {
		return "", err
	}
	want := record{Type: typ, Host: normaliseHost(args[2], domain), Value: strings.TrimSpace(args[3])}
	if typ == "CAA" {
		want.Value = caaValue(want.Value)
	}
	if problem := p.checkHost(want.Host); problem != "" {
		return "", refusal(problem)
	}
	if problem := checkValue(typ, want.Value); problem != "" {
		return "", refusal("Refused: " + problem + ". Nothing was changed.")
	}
	ttlGiven, prefGiven := len(args) >= 5, len(args) >= 6
	if ttlGiven {
		if want.TTL, err = number(args[4], "ttl", minTTL, maxTTL); err != nil {
			return "", err
		}
	}
	if prefGiven {
		if typ != "MX" {
			return "", refusal("Refused: mxpref is for MX records only. Nothing was changed.")
		}
		if want.MXPref, err = number(args[5], "mxpref", 0, 65535); err != nil {
			return "", err
		}
	}

	set, err := p.readSet(domain)
	if err != nil {
		return "", err
	}
	c, err := ensure(domain, set.Records, want, ttlGiven, prefGiven)
	if err != nil {
		return "", err
	}
	return p.apply("ensure", domain, set, c, set.EmailType)
}

// ---- remove ----------------------------------------------------------------------------------

func (p *plugin) remove(args []string) (string, error) {
	domain, err := p.domainArg(args, "remove <domain> <type> <host> [value]", 3, 4)
	if err != nil {
		return "", err
	}
	typ, err := recordType(args[1], true)
	if err != nil {
		return "", err
	}
	host := normaliseHost(args[2], domain)
	if problem := p.checkHost(host); problem != "" {
		return "", refusal(problem)
	}
	value := ""
	if len(args) == 4 {
		value = strings.TrimSpace(args[3])
	}

	set, err := p.readSet(domain)
	if err != nil {
		return "", err
	}
	c, err := remove(set.Records, typ, host, value, len(args) == 4)
	if err != nil {
		return "", err
	}
	if c.none() {
		return fmt.Sprintf("%s: there is no such %s record on %s; nothing was changed.", domain, typ, host), nil
	}
	return p.apply("remove", domain, set, c, set.EmailType)
}

// ---- restore ---------------------------------------------------------------------------------

func (p *plugin) restore(args []string) (string, error) {
	domain, err := p.domainArg(args, "restore <domain> <backup id>", 2, 2)
	if err != nil {
		return "", err
	}
	if !p.cfg.listed(domain) {
		return "", notListed(domain)
	}
	b, err := p.loadBackup(domain, args[1])
	if err != nil {
		return "", err
	}
	set, err := p.readSet(domain)
	if err != nil {
		return "", err
	}
	c := replace(set.Records, b.Records)
	if c.none() {
		return fmt.Sprintf("%s already holds backup %s's records; nothing was changed.", domain, b.ID), nil
	}
	// The restored set builds on the mail setting it was saved with.
	mail := set.EmailType
	if b.EmailType != "" {
		mail = b.EmailType
	}
	return p.apply("restore", domain, set, c, mail)
}

// ---- helpers ---------------------------------------------------------------------------------

// recordType reads a command's type. removing is true for `remove`, which may also take away a
// URL redirect record (URL, URL301 or FRAME); nothing adds or changes one.
func recordType(word string, removing bool) (string, error) {
	t := strings.ToUpper(word)
	if changeable[t] || (removing && redirect[t]) {
		return t, nil
	}
	if redirect[t] {
		return "", refusal(fmt.Sprintf("Refused: %s is a URL redirect record, which this plugin never adds or changes; it may only remove one, with `remove <domain> %s <host> [value]`. Nothing was changed.", word, t))
	}
	return "", refusal(fmt.Sprintf("Refused: %s is not a type this plugin changes; it changes A, AAAA, CNAME, MX, TXT and CAA records and may remove a URL redirect (URL, URL301, FRAME), and keeps every other record as it is. Nothing was changed.", word))
}

func (p *plugin) checkHost(host string) string {
	if host == "@" || hostPattern.MatchString(host) {
		return ""
	}
	return fmt.Sprintf("Refused: %q is not a host; use @ for the domain itself, or a name below it such as www or _dmarc. Nothing was changed.", host)
}

func number(word, name string, min, max int) (int, error) {
	n, err := strconv.Atoi(word)
	if err != nil || n < min || n > max {
		return 0, refusal(fmt.Sprintf("Refused: %s is a whole number from %d to %d. Nothing was changed.", name, min, max))
	}
	return n, nil
}

// errorWords is an error's sentence for setup, without the refusal's own closing words.
func errorWords(err error) string {
	var e *apiError
	if errors.As(err, &e) {
		return e.Error()
	}
	return err.Error()
}
