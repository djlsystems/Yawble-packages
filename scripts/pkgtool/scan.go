package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The scan looks for what identifies a real person in any text file of the repository:
//
//   - an email address at anything but a reserved domain (example.com, example.net, example.org and
//     their subdomains, or the .example, .test, .invalid and .localhost names);
//   - a host or domain - in a URL, or bare without one - that is not reserved, not the project's
//     own github.com/djlsystems, not part of a Go module path a go.mod or go.sum names, and not
//     listed with its reason in the allowed-hosts file (a service a plugin talks to);
//   - an authorship line (copyright, author, signed-off-by and the like) that does not name the
//     project;
//   - any of the people given in the people file: in check.sh, everyone who authored or committed in
//     the repository's history and the git user running it.
//
// A name on its own cannot be told apart from a made-up one by a program; the scan covers the places
// names come with (addresses, domains, authorship, the people who work on the repository), and the
// README asks for made-up people only.

var (
	emailPattern  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@((?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,})`)
	urlPattern    = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://([a-z0-9.-]+)`)
	escapePattern = regexp.MustCompile(`%[0-9A-Fa-f]{2}`)
	// A bare domain must end in a top-level domain people register names under, in lower case, so
	// code such as info.Email or plugin.mail is not taken for one.
	barePattern   = regexp.MustCompile(`[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*\.(?:com|net|org|io|dev|co|ai|app|uk|de|fr|nl|eu|gov|edu|info|biz|xyz|cloud|me|us|ca|au|tech)\b`)
	authorPattern = regexp.MustCompile(`(?i)(\b(copyright|signed-off-by|co-authored-by|reported-by|reviewed-by|authored-by|maintainers?|authors?|contributors?)\b\s*[:=]|"(authors?|maintainers?|contributors)"\s*:|\x{00A9}|\(c\)\s*[0-9]{4})`)
	projectOwner  = "github.com/djlsystems"
)

var reservedSuffixes = []string{".example", ".test", ".invalid", ".localhost"}
var reservedDomains = []string{"example.com", "example.net", "example.org", "localhost"}

func reservedDomain(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, s := range reservedSuffixes {
		if strings.HasSuffix(host, s) || host == s[1:] {
			return true
		}
	}
	for _, d := range reservedDomains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

type scanner struct {
	allowedHosts map[string]bool
	modules      []string // Go module paths named by a go.mod or go.sum
	people       []string // lower case
}

// readAllowedHosts reads one host per line; # starts a comment, which should say why it is allowed.
func readAllowedHosts(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	hosts := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line, _, _ = strings.Cut(line, "#")
		if f := strings.Fields(line); len(f) > 0 {
			hosts[strings.ToLower(f[0])] = true
		}
	}
	return hosts, nil
}

// readPeople reads one name or address per line; anything shorter than four characters is too
// common to look for, and the project's owner is the project, not a person.
func readPeople(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var people []string
	for _, line := range strings.Split(string(data), "\n") {
		p := strings.ToLower(strings.TrimSpace(line))
		if len(p) < 4 || seen[p] || namesTheProject(p) {
			continue
		}
		seen[p] = true
		people = append(people, p)
	}
	return people, nil
}

// scanTree scans every text file under root, leaving out .git, node_modules, zips and binaries, and
// returns one line per finding: "<path>:<line>: <what>".
func scanTree(root, allowedHostsFile, peopleFile string) ([]string, error) {
	hosts, err := readAllowedHosts(allowedHostsFile)
	if err != nil {
		return nil, err
	}
	people, err := readPeople(peopleFile)
	if err != nil {
		return nil, err
	}
	var files []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && (d.Name() == ".git" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() && !strings.HasSuffix(d.Name(), ".zip") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)

	s := scanner{allowedHosts: hosts, people: people}
	for _, f := range files {
		if base := filepath.Base(f); base == "go.mod" || base == "go.sum" {
			s.modules = append(s.modules, modulePaths(f)...)
		}
	}

	var findings []string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			continue // a binary
		}
		rel, _ := filepath.Rel(root, f)
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(nil, 4<<20)
		for n := 1; sc.Scan(); n++ {
			for _, what := range s.line(sc.Text()) {
				findings = append(findings, fmt.Sprintf("%s:%d: %s", filepath.ToSlash(rel), n, what))
			}
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("%s: %v", f, err)
		}
	}
	return findings, nil
}

// modulePaths are the module paths a go.mod names (module and require lines) or a go.sum lists.
func modulePaths(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "require "))
		if len(f) == 0 {
			continue
		}
		switch {
		case f[0] == "module" && len(f) > 1:
			out = append(out, f[1])
		case strings.Contains(f[0], ".") && strings.Contains(f[0], "/") && len(f) > 1:
			out = append(out, f[0])
		}
	}
	return out
}

// line returns what is wrong with one line of text.
func (s scanner) line(text string) []string {
	var found []string
	// %40 and the like are escapes, not part of a name.
	text = escapePattern.ReplaceAllString(text, "   ")
	masked := []byte(text)
	mask := func(from, to int) {
		for i := from; i < to; i++ {
			masked[i] = ' '
		}
	}

	for _, m := range emailPattern.FindAllStringSubmatchIndex(text, -1) {
		if !reservedDomain(text[m[2]:m[3]]) {
			found = append(found, "email address "+text[m[0]:m[1]])
		}
		mask(m[0], m[1])
	}
	for _, m := range urlPattern.FindAllStringSubmatchIndex(string(masked), -1) {
		if what := s.host(text[m[2]:m[3]], text[m[2]:]); what != "" {
			found = append(found, what)
		}
		mask(m[2], m[3])
	}
	for _, m := range barePattern.FindAllStringIndex(string(masked), -1) {
		if what := s.host(text[m[0]:m[1]], text[m[0]:]); what != "" {
			found = append(found, what)
		}
	}

	lower := strings.ToLower(text)
	if m := authorPattern.FindString(text); m != "" && !namesTheProject(lower) {
		found = append(found, "authorship line ("+strings.TrimSpace(m)+") naming someone other than the project")
	}
	withoutProject := strings.ReplaceAll(lower, projectOwner, "")
	for _, p := range s.people {
		if strings.Contains(withoutProject, p) {
			found = append(found, fmt.Sprintf("names %q, who works on this repository", p))
		}
	}
	return found
}

// namesTheProject reports whether a line names the project or its owner, however it is spaced
// ("DJL Systems, Inc.", djlsystems, Yawble).
func namesTheProject(lower string) bool {
	squeezed := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, lower)
	return strings.Contains(squeezed, "djlsystems") || strings.Contains(squeezed, "yawble")
}

// host returns what is wrong with a host found at the start of rest, or "" when it is allowed.
func (s scanner) host(host, rest string) string {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	labels := strings.Split(h, ".")
	if len(labels) < 2 || len(labels[len(labels)-1]) < 2 || strings.Trim(h, "0123456789.") == "" {
		return "" // a single name, a one-letter ending or an IP address: not a registered domain
	}
	if reservedDomain(h) || s.allowedHosts[h] {
		return ""
	}
	if hasPathPrefix(strings.ToLower(rest), projectOwner) {
		return ""
	}
	for _, m := range s.modules {
		if hasPathPrefix(rest, m) {
			return ""
		}
	}
	return "domain " + host + " (not reserved, not the project's, not in the allowed hosts)"
}

// hasPathPrefix reports whether s starts with prefix as a whole path: what follows is not part of
// the same name.
func hasPathPrefix(s, prefix string) bool {
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	rest := s[len(prefix):]
	if rest == "" {
		return true
	}
	nameChar := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
	}
	// A dot carries the name on into a longer one, unless it ends a sentence.
	if rest[0] == '.' {
		return len(rest) == 1 || !nameChar(rest[1])
	}
	return !nameChar(rest[0])
}
