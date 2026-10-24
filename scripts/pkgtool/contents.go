package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// contents is what a package holds, read from the manifests in its zip and never typed: exactly as
// the yawble.ai Marketplace derives it when a zip is uploaded (Yawble-web
// marketplace/admin/package.ts, solutionContents and pluginParts; the shape is CatalogContents in
// marketplace/catalog.ts). A list a package has nothing for is empty, never null.
//
//   - plugins: each plugin a solution ships, the version it ships and minimum, the oldest version it
//     works with: its pluginMinimums entry in solution.json, else the version shipped. A plugin's is
//     empty.
//   - members: an agent member has agent true and role manager or member; a plugin member has agent
//     false and role the id of the plugin it runs.
//   - triggers: each trigger's name and what fires it, in words.
//   - settings, secrets, connections: a plugin's are every one its manifest declares; a solution's
//     are the settings and connections its inputs ask a person for and the keys its members bind,
//     each described from the plugin manifest that declares it.
//   - events: each event a plugin announces (events.publishes).
type contents struct {
	Plugins     []contentsPlugin     `json:"plugins"`
	Members     []contentsMember     `json:"members"`
	Skills      []string             `json:"skills"`
	Triggers    []contentsTrigger    `json:"triggers"`
	Sites       []string             `json:"sites"`
	Settings    []contentsSetting    `json:"settings"`
	Secrets     []contentsSecret     `json:"secrets"`
	Connections []contentsConnection `json:"connections"`
	Events      []contentsEvent      `json:"events"`
}

type contentsPlugin struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Minimum string `json:"minimum"`
}

type contentsMember struct {
	Name  string `json:"name"`
	Role  string `json:"role"`
	Agent bool   `json:"agent"`
}

type contentsTrigger struct {
	Name  string `json:"name"`
	Words string `json:"words"`
}

type contentsSetting struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Kind     string `json:"kind"`
	Required bool   `json:"required"`
	Default  string `json:"default"`
}

type contentsSecret struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Required bool   `json:"required"`
	Masked   bool   `json:"masked"`
}

type contentsConnection struct {
	Slot      string   `json:"slot"`
	Providers []string `json:"providers"`
	Required  bool     `json:"required"`
}

type contentsEvent struct {
	Type    string `json:"type"`
	Summary string `json:"summary"`
}

func emptyContents() contents {
	return contents{
		Plugins: []contentsPlugin{}, Members: []contentsMember{}, Skills: []string{}, Triggers: []contentsTrigger{},
		Sites: []string{}, Settings: []contentsSetting{}, Secrets: []contentsSecret{}, Connections: []contentsConnection{},
		Events: []contentsEvent{},
	}
}

// jsonObject is a JSON object read in the order written.
type jsonObject struct {
	keys   []string
	values map[string]json.RawMessage
}

func (o jsonObject) get(key string) json.RawMessage { return o.values[key] }

func (o jsonObject) has(key string) bool {
	_, ok := o.values[key]
	return ok
}

// asObject reads raw as an object; null or absent is an empty one, anything else not an object is
// refused, as the Marketplace refuses it.
func asObject(raw json.RawMessage, where string) (jsonObject, error) {
	if isAbsent(raw) {
		return jsonObject{values: map[string]json.RawMessage{}}, nil
	}
	if !isObjectJSON(raw) {
		return jsonObject{}, fmt.Errorf("%s is not an object", where)
	}
	keys, values, err := orderedObject[json.RawMessage](raw)
	if err != nil {
		return jsonObject{}, fmt.Errorf("%s: %v", where, err)
	}
	return jsonObject{keys, values}, nil
}

// asList reads raw as a list; null or absent is an empty one.
func asList(raw json.RawMessage, where string) ([]json.RawMessage, error) {
	if isAbsent(raw) {
		return nil, nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("%s is not a list", where)
	}
	return list, nil
}

func isAbsent(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "null"
}

func isObjectJSON(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '{'
}

// text is raw when it is a JSON string, else "" and false.
func text(raw json.RawMessage) (string, bool) {
	var s string
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '"' || json.Unmarshal(t, &s) != nil {
		return "", false
	}
	return s, true
}

func textOr(raw json.RawMessage, fallback string) string {
	if s, ok := text(raw); ok {
		return s
	}
	return fallback
}

func isTrue(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "true" }

// field reads key of raw when raw is an object, else nothing.
func field(raw json.RawMessage, key string) json.RawMessage {
	if !isObjectJSON(raw) {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m[key]
}

// textList is the non-empty strings of a list, anything else in it left out.
func textList(raw json.RawMessage) []string {
	out := []string{}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return out
	}
	for _, x := range list {
		if s, ok := text(x); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// defaultInWords is a setting's default as the Marketplace writes it: a string as it is, On or Off,
// a number as JavaScript prints it, a list's items joined by ", ", nothing as "".
func defaultInWords(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return ""
	}
	switch t[0] {
	case '"':
		s, _ := text(t)
		return s
	case 't':
		return "On"
	case 'f':
		return "Off"
	case '[':
		var items []json.RawMessage
		_ = json.Unmarshal(t, &items)
		var words []string
		for _, item := range items {
			if w := defaultInWords(item); w != "" {
				words = append(words, w)
			}
		}
		return strings.Join(words, ", ")
	case '{':
		var b bytes.Buffer
		if json.Compact(&b, t) == nil {
			return b.String()
		}
		return string(t)
	}
	f, err := strconv.ParseFloat(string(t), 64)
	if err != nil {
		return string(t)
	}
	return jsNumber(f)
}

// jsNumber is a number as JavaScript's String(n) writes it.
func jsNumber(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	if a := math.Abs(f); a >= 1e-6 && a < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	mant, exp, _ := strings.Cut(s, "e")
	if e := strings.TrimLeft(exp[1:], "0"); exp[0] == '-' {
		return mant + "e-" + e
	} else {
		return mant + "e+" + e
	}
}

func settingOf(name string, f json.RawMessage) contentsSetting {
	return contentsSetting{
		Name:     name,
		Label:    textOr(field(f, "label"), ""),
		Kind:     textOr(field(f, "type"), ""),
		Required: isTrue(field(f, "required")),
		Default:  defaultInWords(field(f, "default")),
	}
}

func secretOf(name string, f json.RawMessage) contentsSecret {
	return contentsSecret{
		Name:     name,
		Label:    textOr(field(f, "label"), ""),
		Required: isTrue(field(f, "required")),
		Masked:   string(bytes.TrimSpace(field(f, "masked"))) != "false",
	}
}

// pluginContents is what one plugin manifest holds: every setting, secret and connection it declares,
// its skills and the events it announces.
func pluginContents(manifest json.RawMessage) (contents, error) {
	c := emptyContents()
	id := textOr(field(manifest, "id"), "")
	config, err := asObject(field(manifest, "config"), "plugin "+id+` "config"`)
	if err != nil {
		return c, err
	}
	for _, name := range config.keys {
		c.Settings = append(c.Settings, settingOf(name, config.get(name)))
	}
	secrets, err := asObject(field(manifest, "secrets"), "plugin "+id+` "secrets"`)
	if err != nil {
		return c, err
	}
	for _, name := range secrets.keys {
		c.Secrets = append(c.Secrets, secretOf(name, secrets.get(name)))
	}
	conns, err := asObject(field(manifest, "connections"), "plugin "+id+` "connections"`)
	if err != nil {
		return c, err
	}
	for _, slot := range conns.keys {
		s := conns.get(slot)
		c.Connections = append(c.Connections, contentsConnection{slot, textList(field(s, "providers")), isTrue(field(s, "required"))})
	}
	// A plugin names each skill by its file, skills/<name>.md.
	skills, err := asList(field(manifest, "skills"), "plugin "+id+` "skills"`)
	if err != nil {
		return c, err
	}
	for _, s := range skills {
		name, ok := text(s)
		if !ok {
			name = textOr(field(s, "name"), "")
		}
		name = name[strings.LastIndex(name, "/")+1:]
		if strings.HasSuffix(strings.ToLower(name), ".md") {
			name = name[:len(name)-3]
		}
		if name != "" {
			c.Skills = append(c.Skills, name)
		}
	}
	events, err := asList(field(field(manifest, "events"), "publishes"), "plugin "+id+` "events.publishes"`)
	if err != nil {
		return c, err
	}
	for _, e := range events {
		if t, ok := text(field(e, "type")); ok && t != "" {
			c.Events = append(c.Events, contentsEvent{t, textOr(field(e, "summary"), "")})
		}
	}
	return c, nil
}

// solutionContents is what a solution holds, from solution.json and the manifests of the plugins it
// ships, in the order the zip holds them (plugins/<id>/ sorted). Its settings and sign-ins are the
// ones inputs asks a person for; its keys are the ones its members bind, once each; each is
// described by the plugin manifest that declares it.
func solutionContents(solution json.RawMessage, plugins []json.RawMessage) (contents, error) {
	c := emptyContents()
	byID := map[string]json.RawMessage{}
	minimums := jsonObject{values: map[string]json.RawMessage{}}
	if m := field(solution, "pluginMinimums"); isObjectJSON(m) {
		minimums, _ = asObject(m, "solution.json \"pluginMinimums\"")
	}
	for _, p := range plugins {
		id, version := textOr(field(p, "id"), ""), textOr(field(p, "version"), "")
		byID[id] = p
		minimum := version
		if s, ok := text(minimums.get(id)); ok && s != "" {
			minimum = s
		}
		c.Plugins = append(c.Plugins, contentsPlugin{id, version, minimum})
	}

	members, err := asList(field(solution, "members"), `solution.json "members"`)
	if err != nil {
		return c, err
	}
	memberPlugin := map[string]json.RawMessage{}
	for _, m := range members {
		name := textOr(field(m, "name"), "")
		if pluginID, ok := text(field(m, "pluginId")); ok && pluginID != "" {
			c.Members = append(c.Members, contentsMember{name, pluginID, false})
			if p, ok := byID[pluginID]; ok {
				memberPlugin[strings.ToLower(name)] = p
			}
			continue
		}
		role := "member"
		if textOr(field(m, "role"), "") == "manager" {
			role = "manager"
		}
		c.Members = append(c.Members, contentsMember{name, role, true})
	}
	c.Skills = textList(field(solution, "skills"))
	triggers, err := asList(field(solution, "triggers"), `solution.json "triggers"`)
	if err != nil {
		return c, err
	}
	for _, t := range triggers {
		c.Triggers = append(c.Triggers, contentsTrigger{textOr(field(t, "name"), ""), triggerInWords(t)})
	}
	c.Sites = textList(field(solution, "sites"))

	inputs := field(solution, "inputs")
	settings, err := asList(field(inputs, "settings"), `solution.json "inputs.settings"`)
	if err != nil {
		return c, err
	}
	for _, st := range settings {
		setting, member := textOr(field(st, "setting"), ""), strings.ToLower(textOr(field(st, "member"), ""))
		var declared json.RawMessage
		if p, ok := memberPlugin[member]; ok {
			declared = field(field(p, "config"), setting)
		}
		value := field(declared, "default")
		for _, m := range members {
			if strings.ToLower(textOr(field(m, "name"), "")) != member {
				continue
			}
			if own := field(m, "settings"); isObjectJSON(own) {
				if o, err := asObject(own, "settings"); err == nil && o.has(setting) {
					value = o.get(setting)
				}
			}
			break
		}
		s := settingOf(setting, declared)
		s.Required, s.Default = isTrue(field(st, "required")), defaultInWords(value)
		c.Settings = append(c.Settings, s)
	}

	bound := map[string]bool{}
	for _, m := range members {
		pluginID, _ := text(field(m, "pluginId"))
		if pluginID == "" {
			continue
		}
		declared := field(byID[pluginID], "secrets")
		binds, err := asObject(field(m, "secrets"), "solution.json member "+textOr(field(m, "name"), "")+` "secrets"`)
		if err != nil {
			return c, err
		}
		for _, f := range binds.keys {
			key, _ := text(binds.get(f))
			if bound[key] {
				continue
			}
			bound[key] = true
			d := field(declared, f)
			s := secretOf(key, d)
			s.Required = isAbsent(field(d, "when"))
			c.Secrets = append(c.Secrets, s)
		}
	}

	conns, err := asList(field(inputs, "connections"), `solution.json "inputs.connections"`)
	if err != nil {
		return c, err
	}
	for _, cn := range conns {
		slot := textOr(field(cn, "slot"), "")
		p := memberPlugin[strings.ToLower(textOr(field(cn, "member"), ""))]
		providers := []string{}
		if raw := field(field(field(p, "connections"), slot), "providers"); !isAbsent(raw) {
			if err := json.Unmarshal(raw, &providers); err != nil {
				return c, fmt.Errorf("plugin connections.%s.providers is not a list of names", slot)
			}
		}
		c.Connections = append(c.Connections, contentsConnection{slot, providers, isTrue(field(cn, "required"))})
	}
	return c, nil
}

var (
	filterPattern      = regexp.MustCompile(`^(\S+)\s+(eq|contains)\s+(.+)$`)
	pluginEventPattern = regexp.MustCompile(`^plugin\.([^.]+)\.(.+)$`)
	plainCronField     = regexp.MustCompile(`^\d{1,2}$`)
	dayRangePattern    = regexp.MustCompile(`^([0-6])-([0-6])$`)
	dayListPattern     = regexp.MustCompile(`^[0-7](,[0-7])*$`)
	trailingSlashes    = regexp.MustCompile(`/+$`)
	dayNames           = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	durationUnits      = []struct {
		size int64
		name string
	}{{86400, "day"}, {3600, "hour"}, {60, "minute"}, {1, "second"}}
)

// triggerInWords is what fires a trigger, in words: "every 5 minutes", "when a file arrives in
// Resume/", "when the mail plugin announces received".
func triggerInWords(t json.RawMessage) string {
	switch textOr(field(t, "kind"), "") {
	case "schedule":
		schedule := field(t, "schedule")
		var every float64
		if raw := field(schedule, "everySeconds"); json.Unmarshal(raw, &every) == nil && !isAbsent(raw) && every == math.Trunc(every) && every > 0 {
			return "every " + durationInWords(int64(every))
		}
		if cron, ok := text(field(schedule, "cron")); ok {
			tz := textOr(field(schedule, "timezone"), "")
			if tz == "" {
				tz = "UTC"
			}
			return cronInWords(cron, tz)
		}
		return "on a schedule"
	case "folder":
		folder := field(t, "folder")
		path := trailingSlashes.ReplaceAllString(textOr(field(folder, "path"), ""), "") + "/"
		glob := textOr(field(folder, "glob"), "")
		if glob == "" || glob == "*" {
			return "when a file arrives in " + path
		}
		return "when a file matching " + glob + " arrives in " + path
	case "event":
		event := field(t, "event")
		typ := textOr(field(event, "type"), "")
		var filter []string
		if f, ok := text(field(event, "filter")); ok {
			filter = filterPattern.FindStringSubmatch(strings.TrimSpace(f))
		}
		if typ == "site.action" && filter != nil && filter[1] == "siteAction" && filter[2] == "eq" && strings.Contains(filter[3], "/") {
			site, action, _ := strings.Cut(filter[3], "/")
			return "when someone presses " + action + " on the " + site + " site"
		}
		where := ""
		if filter != nil {
			op := "containing"
			if filter[2] == "eq" {
				op = "is"
			}
			where = " with " + filter[1] + " " + op + " " + filter[3]
		}
		if p := pluginEventPattern.FindStringSubmatch(typ); p != nil {
			return "when the " + p[1] + " plugin announces " + p[2] + where
		}
		return "when " + typ + " happens" + where
	}
	return "when it is fired"
}

func durationInWords(seconds int64) string {
	for _, u := range durationUnits {
		if seconds%u.size == 0 {
			if n := seconds / u.size; n != 1 {
				return strconv.FormatInt(n, 10) + " " + u.name + "s"
			}
			return u.name
		}
	}
	return ""
}

// cronInWords is a six-field cron (seconds first) in words when it is a time of day on some days;
// else the cron itself.
func cronInWords(cron, timezone string) string {
	f := strings.Fields(cron)
	if len(f) == 6 && plainCronField.MatchString(f[1]) && plainCronField.MatchString(f[2]) && f[3] == "*" && f[4] == "*" && (f[0] == "0" || f[0] == "00") {
		at := padTwo(f[2]) + ":" + padTwo(f[1])
		if days, ok := daysInWords(f[5]); ok {
			return days + " at " + at + " (" + timezone + ")"
		}
	}
	return "on the schedule " + strings.TrimSpace(cron) + " (" + timezone + ")"
}

func padTwo(s string) string {
	if len(s) < 2 {
		return "0" + s
	}
	return s
}

func daysInWords(f string) (string, bool) {
	if f == "*" || f == "?" {
		return "every day", true
	}
	if r := dayRangePattern.FindStringSubmatch(f); r != nil {
		a, _ := strconv.Atoi(r[1])
		b, _ := strconv.Atoi(r[2])
		return dayNames[a] + " to " + dayNames[b], true
	}
	if dayListPattern.MatchString(f) {
		var names []string
		for _, d := range strings.Split(f, ",") {
			n, _ := strconv.Atoi(d)
			names = append(names, dayNames[n%7]+"s")
		}
		if len(names) == 1 {
			return "on " + names[0], true
		}
		return "on " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1], true
	}
	return "", false
}
