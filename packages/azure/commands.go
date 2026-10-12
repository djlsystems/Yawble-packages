package main

import (
	"fmt"
	"sort"
	"strings"
)

// command is one entry of the dispatcher. Each command file registers its own in an init, so a new
// group of commands slots in without touching this file.
type command struct {
	words []string // the command's own words, lower case: "rg", "create"
	usage string   // its arguments, as the usage line shows them
	run   func(p *plugin, args []string) (string, error)
}

var commands []command

func register(cs ...command) { commands = append(commands, cs...) }

// match finds the command a line starts with, the longest first, and answers its arguments.
func match(line string) (*command, []string, error) {
	words, err := split(line)
	if err != nil {
		return nil, nil, err
	}
	var best *command
	for i := range commands {
		c := &commands[i]
		if len(c.words) > len(words) || (best != nil && len(c.words) <= len(best.words)) {
			continue
		}
		ok := true
		for j, w := range c.words {
			if !strings.EqualFold(words[j], w) {
				ok = false
				break
			}
		}
		if ok {
			best = c
		}
	}
	if best == nil {
		return nil, nil, refusal(usage(line))
	}
	return best, words[len(best.words):], nil
}

// split cuts a line into words; a value with spaces is quoted, "like this" or 'like this'.
func split(line string) ([]string, error) {
	var words []string
	var cur strings.Builder
	quote, inWord := rune(0), false
	for _, r := range line {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
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
		return nil, refusef("Refused: a quote is not closed in %q.", line)
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// commandName is the first word of a line, for a refusal of a line no command matched.
func commandName(line string) string {
	if f := strings.Fields(line); len(f) > 0 {
		return strings.ToLower(f[0])
	}
	return ""
}

// usage lists every command, after saying what was not understood.
func usage(line string) string {
	var lines []string
	for _, c := range commands {
		lines = append(lines, strings.TrimSpace("- "+strings.Join(c.words, " ")+" "+c.usage))
	}
	sort.Strings(lines)
	head := "Refused: no instruction was given."
	if line != "" {
		head = fmt.Sprintf("Refused: %q is not a command this member knows.", commandName(line))
	}
	return head + " One command per line:\n" + strings.Join(lines, "\n")
}

// argsExactly refuses a command given the wrong number of arguments, quoting its usage.
func argsExactly(args []string, min, max int, words, use string) error {
	if len(args) < min || len(args) > max {
		return refusef("Refused: %s takes %s.", words, use)
	}
	return nil
}
