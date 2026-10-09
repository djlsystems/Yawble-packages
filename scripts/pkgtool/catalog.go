package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// schema is the catalog's format number. Raise it on any change a reader must know about; see
// docs/catalog.md.
const schema = 1

// repository is where the packages are published and their sources live.
const repository = "https://github.com/djlsystems/Yawble-packages"

var (
	// The date part is checked again as a calendar date; n counts from 1 with no leading zero.
	tagPattern  = regexp.MustCompile(`^catalog-([0-9]{4}\.[0-9]{2}\.[0-9]{2})\.[1-9][0-9]*$`)
	htmlPattern = regexp.MustCompile(`<[A-Za-z/!]`)
	// A sentence ends at . ! or ? followed by a space and a capital letter, or at the end.
	sentenceEnd = regexp.MustCompile(`[.!?]\s+[A-Z]`)
)

type catalog struct {
	Schema      int     `json:"schema"`
	GeneratedAt string  `json:"generatedAt"`
	Packages    []entry `json:"packages"`
}

type entry struct {
	ID          string      `json:"id"`
	Kind        string      `json:"kind"`
	Name        string      `json:"name"`
	Version     string      `json:"version"`
	Summary     string      `json:"summary"`
	Description string      `json:"description"`
	Needs       needs       `json:"needs"`
	Plugins     []pluginRef `json:"plugins"`
	Platforms   []string    `json:"platforms"`
	Download    download    `json:"download"`
	Source      string      `json:"source"`
}

type needs struct {
	Connections []connectionNeed `json:"connections"`
	Secrets     []secretNeed     `json:"secrets"`
	Inputs      []inputNeed      `json:"inputs"`
	Runtimes    []string         `json:"runtimes"`
}

type connectionNeed struct {
	Slot      string   `json:"slot"`
	Providers []string `json:"providers"`
	Required  bool     `json:"required"`
	Why       string   `json:"why"`
}

type secretNeed struct {
	Key  string `json:"key"`
	Why  string `json:"why"`
	When string `json:"when,omitempty"`
}

type inputNeed struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Required bool   `json:"required"`
	Why      string `json:"why"`
}

type pluginRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type download struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// writeCatalog reads every <dist>/*.zip - exactly what a person downloads - and writes
// <dist>/catalog.json. Nothing in it is typed anywhere else: each field comes from a manifest inside
// the zip, or from the zip itself.
func writeCatalog(tag, dist string, at time.Time) error {
	if err := checkTag(tag); err != nil {
		return err
	}
	zips, err := filepath.Glob(filepath.Join(dist, "*.zip"))
	if err != nil {
		return err
	}
	if len(zips) == 0 {
		return fmt.Errorf("%s holds no zips; build the packages first", dist)
	}
	sort.Strings(zips)
	c := catalog{Schema: schema, GeneratedAt: at.UTC().Format(generatedAtLayout), Packages: []entry{}}
	seen := map[string]string{}
	for _, z := range zips {
		e, err := readEntry(z, tag)
		if err != nil {
			return fmt.Errorf("%s: %v", filepath.Base(z), err)
		}
		if other, dup := seen[e.ID]; dup {
			return fmt.Errorf("%s and %s are both package %q; keep one version per build", other, filepath.Base(z), e.ID)
		}
		seen[e.ID] = filepath.Base(z)
		c.Packages = append(c.Packages, e)
	}
	sort.Slice(c.Packages, func(i, j int) bool { return c.Packages[i].ID < c.Packages[j].ID })
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dist, "catalog.json"), append(data, '\n'), 0o644)
}

// generatedAtLayout is how the catalog writes generatedAt: UTC, ISO 8601, to the second.
const generatedAtLayout = "2006-01-02T15:04:05Z"

// checkTag refuses a release tag that is not catalog-<yyyy.mm.dd>.<n> with a real calendar date and
// n a number from 1 with no leading zero. scripts/build.sh and scripts/publish.sh both ask this.
func checkTag(tag string) error {
	m := tagPattern.FindStringSubmatch(tag)
	if m == nil {
		return fmt.Errorf("tag %q is not catalog-<yyyy.mm.dd>.<n> with n from 1, such as catalog-2026.10.08.1", tag)
	}
	if d, err := time.Parse("2006.01.02", m[1]); err != nil || d.Year() < 1 {
		return fmt.Errorf("tag %q: %s is not a calendar date", tag, m[1])
	}
	return nil
}

// zipFiles is a zip's entries by name.
type zipFiles map[string]*zip.File

func (z zipFiles) readJSON(name string, v any) error {
	f, ok := z[name]
	if !ok {
		return fmt.Errorf("%s is missing", name)
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %v", name, err)
	}
	return nil
}

func readEntry(zipPath, tag string) (entry, error) {
	data, err := os.ReadFile(zipPath)
	if err != nil {
		return entry{}, err
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return entry{}, err
	}
	files := zipFiles{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	sum := sha256.Sum256(data)

	var e entry
	if _, ok := files["solution.json"]; ok {
		e, err = solutionEntry(files)
	} else if _, ok := files["plugin.json"]; ok {
		e, err = pluginEntry(files)
	} else {
		return entry{}, fmt.Errorf("holds neither solution.json nor plugin.json at its root")
	}
	if err != nil {
		return entry{}, err
	}
	name := filepath.Base(zipPath)
	if want := e.ID + "-" + e.Version + ".zip"; name != want {
		return entry{}, fmt.Errorf("its manifest says %s %s, so it must be named %s", e.ID, e.Version, want)
	}
	if htmlPattern.MatchString(e.Description) {
		return entry{}, fmt.Errorf("the manifest's description holds HTML; the catalog's description is plain text")
	}
	e.Summary = firstSentence(e.Description)
	e.Download = download{
		URL:    repository + "/releases/download/" + tag + "/" + name,
		SHA256: hex.EncodeToString(sum[:]),
		Bytes:  int64(len(data)),
	}
	e.Source = repository + "/tree/main/packages/" + e.ID
	return e, nil
}

func newEntry(kind, id, name, version, description string) entry {
	return entry{
		ID: id, Kind: kind, Name: name, Version: version,
		Description: strings.TrimSpace(description),
		Needs: needs{
			Connections: []connectionNeed{}, Secrets: []secretNeed{}, Inputs: []inputNeed{}, Runtimes: []string{},
		},
		Plugins:   []pluginRef{},
		Platforms: []string{},
	}
}

// builtPlugin is one built plugin version inside a zip, at prefix ("" for a plugin zip,
// "plugins/<id>/" in a solution zip).
type builtPlugin struct {
	manifest  pluginManifest
	platforms []string
}

func readPlugin(files zipFiles, prefix string) (builtPlugin, error) {
	var m pluginManifest
	if err := files.readJSON(prefix+"plugin.json", &m); err != nil {
		return builtPlugin{}, err
	}
	if prefix != "" && prefix != "plugins/"+m.ID+"/" {
		return builtPlugin{}, fmt.Errorf("%splugin.json says id %q; its folder must be named after it", prefix, m.ID)
	}
	keys, paths, err := orderedObject[string](m.Platforms)
	if err != nil {
		return builtPlugin{}, fmt.Errorf("%splugin.json platforms: %v", prefix, err)
	}
	for _, k := range keys {
		if _, ok := files[prefix+paths[k]]; !ok {
			return builtPlugin{}, fmt.Errorf("%splugin.json names %s for %s, which the build did not make", prefix, paths[k], k)
		}
	}
	if _, ok := files[prefix+m.Executable.Path]; !ok {
		return builtPlugin{}, fmt.Errorf("%splugin.json's executable %s is not in the zip", prefix, m.Executable.Path)
	}
	return builtPlugin{manifest: m, platforms: keys}, nil
}

func pluginEntry(files zipFiles) (entry, error) {
	p, err := readPlugin(files, "")
	if err != nil {
		return entry{}, err
	}
	m := p.manifest
	e := newEntry("plugin", m.ID, m.Name, m.Version, m.Description)

	slots, conns, err := orderedObject[connectionSlot](m.Connections)
	if err != nil {
		return entry{}, fmt.Errorf("plugin.json connections: %v", err)
	}
	for _, s := range slots {
		c := conns[s]
		e.Needs.Connections = append(e.Needs.Connections, connectionNeed{s, nonNil(c.Providers), c.Required, c.Description})
	}
	// A plugin package binds no key names: each secret is listed by the manifest's field, which a
	// person binds to a key of their choosing when they hire the plugin.
	fields, secrets, err := orderedObject[secretField](m.Secrets)
	if err != nil {
		return entry{}, fmt.Errorf("plugin.json secrets: %v", err)
	}
	for _, f := range fields {
		when, err := secretWhen(f, secrets[f].When, m.Config)
		if err != nil {
			return entry{}, fmt.Errorf("plugin.json %v", err)
		}
		e.Needs.Secrets = append(e.Needs.Secrets, secretNeed{f, secrets[f].Description, when})
	}
	settings, config, err := orderedObject[configField](m.Config)
	if err != nil {
		return entry{}, fmt.Errorf("plugin.json config: %v", err)
	}
	for _, s := range settings {
		if f := config[s]; f.SetBy == "person" {
			e.Needs.Inputs = append(e.Needs.Inputs, inputNeed{s, "setting", f.Required, f.Description})
		}
	}
	e.Needs.Runtimes = sortedUnique(m.Requires)
	e.Plugins = []pluginRef{{m.ID, m.Version}}
	e.Platforms = nonNil(p.platforms)
	return e, nil
}

func solutionEntry(files zipFiles) (entry, error) {
	var s solutionManifest
	if err := files.readJSON("solution.json", &s); err != nil {
		return entry{}, err
	}
	e := newEntry("solution", s.ID, s.Name, s.Version, s.Description)

	var prefixes []string
	for name := range files {
		if dir, file := filepath.Split(name); file == "plugin.json" && strings.Count(dir, "/") == 2 && strings.HasPrefix(dir, "plugins/") {
			prefixes = append(prefixes, dir)
		}
	}
	sort.Strings(prefixes)
	plugins := map[string]builtPlugin{}
	var runtimes []string
	var platforms []string
	declared := false
	for _, prefix := range prefixes {
		p, err := readPlugin(files, prefix)
		if err != nil {
			return entry{}, err
		}
		plugins[p.manifest.ID] = p
		e.Plugins = append(e.Plugins, pluginRef{p.manifest.ID, p.manifest.Version})
		runtimes = append(runtimes, p.manifest.Requires...)
		// A plugin without `platforms` (a script) runs wherever its runtime does and narrows
		// nothing; the package runs on the platforms every plugin that has binaries was built for.
		if len(p.platforms) > 0 {
			if !declared {
				platforms, declared = p.platforms, true
			} else {
				platforms = intersect(platforms, p.platforms)
			}
		}
	}
	e.Needs.Runtimes = sortedUnique(runtimes)
	e.Platforms = nonNil(platforms)

	memberPlugin := map[string]builtPlugin{}
	for _, m := range s.Members {
		if m.PluginID == "" {
			continue
		}
		p, ok := plugins[m.PluginID]
		if !ok {
			return entry{}, fmt.Errorf("solution.json member %s is plugin %s, which the zip does not hold under plugins/", m.Name, m.PluginID)
		}
		memberPlugin[strings.ToLower(m.Name)] = p
	}

	for _, c := range s.Inputs.Connections {
		p, ok := memberPlugin[strings.ToLower(c.Member)]
		if !ok {
			return entry{}, fmt.Errorf("solution.json inputs.connections names %s, which is not a plugin member", c.Member)
		}
		_, slots, err := orderedObject[connectionSlot](p.manifest.Connections)
		if err != nil {
			return entry{}, err
		}
		slot, ok := slots[c.Slot]
		if !ok {
			return entry{}, fmt.Errorf("solution.json inputs.connections slot %s is not a slot of plugin %s", c.Slot, p.manifest.ID)
		}
		e.Needs.Connections = append(e.Needs.Connections, connectionNeed{c.Slot, nonNil(slot.Providers), c.Required, c.Description})
	}

	keys := map[string]int{}
	for _, m := range s.Members {
		if m.PluginID == "" {
			continue
		}
		fields, bound, err := orderedObject[string](m.Secrets)
		if err != nil {
			return entry{}, fmt.Errorf("solution.json member %s secrets: %v", m.Name, err)
		}
		_, declaredSecrets, err := orderedObject[secretField](plugins[m.PluginID].manifest.Secrets)
		if err != nil {
			return entry{}, err
		}
		for _, f := range fields {
			d, ok := declaredSecrets[f]
			if !ok {
				return entry{}, fmt.Errorf("solution.json member %s binds secret %s, which plugin %s does not declare", m.Name, f, m.PluginID)
			}
			when, err := secretWhen(f, d.When, plugins[m.PluginID].manifest.Config)
			if err != nil {
				return entry{}, fmt.Errorf("plugin %s %v", m.PluginID, err)
			}
			key := bound[f]
			i, seen := keys[key]
			if !seen {
				keys[key] = len(e.Needs.Secrets)
				e.Needs.Secrets = append(e.Needs.Secrets, secretNeed{key, d.Description, when})
				continue
			}
			// A key bound more than once is needed whenever any binding needs it: always, when one
			// binding always needs it.
			if had := e.Needs.Secrets[i].When; had == "" || when == "" {
				e.Needs.Secrets[i].When = ""
			} else if had != when {
				e.Needs.Secrets[i].When = had + ", or " + when
			}
		}
	}

	for _, d := range s.Inputs.Documents {
		e.Needs.Inputs = append(e.Needs.Inputs, inputNeed{d.Folder, "documents", d.Required, d.Description})
	}
	for _, st := range s.Inputs.Settings {
		e.Needs.Inputs = append(e.Needs.Inputs, inputNeed{st.Setting, "setting", st.Required, st.Description})
	}
	return e, nil
}

// secretWhen puts a plugin secret's when - {"<setting>": "<value>"}, one pair - in plain words:
// "when the sources setting includes adzuna" for a list setting, "when the mode setting is send" for
// a choice. No when is "": the secret is always needed. A when the Host would not accept is refused
// rather than guessed at.
func secretWhen(field string, raw, config json.RawMessage) (string, error) {
	if t := bytes.TrimSpace(raw); len(t) == 0 || string(t) == "null" {
		return "", nil
	}
	var when map[string]string
	if err := json.Unmarshal(raw, &when); err != nil {
		return "", fmt.Errorf("secret %s: when: %v", field, err)
	}
	if len(when) != 1 {
		return "", fmt.Errorf("secret %s: when must name exactly one setting", field)
	}
	_, settings, err := orderedObject[configField](config)
	if err != nil {
		return "", fmt.Errorf("config: %v", err)
	}
	for name, value := range when {
		s, ok := settings[name]
		if !ok {
			return "", fmt.Errorf("secret %s: when names %s, which is not a setting", field, name)
		}
		if !slices.Contains(s.Enum, value) {
			return "", fmt.Errorf("secret %s: when %s is %s, which is not one of its values", field, name, value)
		}
		switch s.Type {
		case "list":
			return "when the " + name + " setting includes " + value, nil
		case "string":
			return "when the " + name + " setting is " + value, nil
		}
		return "", fmt.Errorf("secret %s: when names %s, which is not a list or choice setting", field, name)
	}
	return "", nil
}

// firstSentence is the description up to its first sentence's end: the catalog's summary.
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if para, _, ok := strings.Cut(s, "\n\n"); ok {
		s = para
	}
	if loc := sentenceEnd.FindStringIndex(s); loc != nil {
		s = s[:loc[0]+1]
	}
	return strings.Join(strings.Fields(s), " ")
}

func intersect(a, b []string) []string {
	in := map[string]bool{}
	for _, x := range b {
		in[x] = true
	}
	out := []string{}
	for _, x := range a {
		if in[x] {
			out = append(out, x)
		}
	}
	return out
}

func sortedUnique(xs []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}
