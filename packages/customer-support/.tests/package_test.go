// Checks of the Customer Support package as files: solution.json against the published mail and
// twilio plugins' manifests, the team skill's rules, the sites and the plugin builds. Nothing here
// reaches a mailbox or Twilio.
//
//	(cd packages/customer-support/.tests && go test -count=1 ./...)
package customersupporttest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// pkgDir is the package folder; the published Mail and Twilio packages sit beside it.
const pkgDir = ".."

type trigger struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Schedule *struct {
		EverySeconds int    `json:"everySeconds"`
		Cron         string `json:"cron"`
	} `json:"schedule"`
	Event *struct {
		Type   string `json:"type"`
		Filter string `json:"filter"`
	} `json:"event"`
	Member        string `json:"member"`
	Instruction   string `json:"instruction"`
	WakeManager   string `json:"wakeManager"`
	DailyTokenCap *int   `json:"dailyTokenCap"`
	RunAtInstall  bool   `json:"runAtInstall"`
	IdleOnly      *bool  `json:"idleOnly"`
}

type memberRef struct {
	Member   string          `json:"member"`
	Setting  string          `json:"setting"`
	Slot     string          `json:"slot"`
	Default  json.RawMessage `json:"default"`
	Question string          `json:"question"`
	Hint     string          `json:"hint"`
	Required bool            `json:"required"`
}

type solution struct {
	Format  int    `json:"format"`
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Members []struct {
		Name         string            `json:"name"`
		Role         string            `json:"role"`
		PluginID     string            `json:"pluginId"`
		Instructions string            `json:"instructions"`
		Settings     map[string]any    `json:"settings"`
		Secrets      map[string]string `json:"secrets"`
	} `json:"members"`
	Triggers []trigger `json:"triggers"`
	Skills   []string  `json:"skills"`
	Sites    []string  `json:"sites"`
	Inputs   struct {
		Settings    []memberRef `json:"settings"`
		Connections []memberRef `json:"connections"`
	} `json:"inputs"`
	Setup struct {
		Welcome     string      `json:"welcome"`
		Needs       string      `json:"needs"`
		Connections []memberRef `json:"connections"`
		Settings    []memberRef `json:"settings"`
	} `json:"setup"`
	Panel struct {
		PrimarySite string      `json:"primarySite"`
		Settings    []memberRef `json:"settings"`
		Status      string      `json:"status"`
	} `json:"panel"`
}

type plugin struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Config  map[string]struct {
		Type    string          `json:"type"`
		Enum    []string        `json:"enum"`
		Default json.RawMessage `json:"default"`
		SetBy   string          `json:"setBy"`
	} `json:"config"`
	Secrets     map[string]json.RawMessage          `json:"secrets"`
	Connections map[string]json.RawMessage          `json:"connections"`
	Reads       []struct{ Site, Collection string } `json:"reads"`
	Events      struct {
		Publishes []struct {
			Type   string `json:"type"`
			Fields []struct {
				Name string `json:"name"`
			} `json:"fields"`
		} `json:"publishes"`
	} `json:"events"`
}

func readJSON(t *testing.T, path string, v any) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return data
}

func loadSolution(t *testing.T) (solution, string) {
	t.Helper()
	var s solution
	raw := readJSON(t, filepath.Join(pkgDir, "solution.json"), &s)
	return s, string(raw)
}

// plugins answers the published plugins this package hires, by id.
func plugins(t *testing.T) map[string]plugin {
	t.Helper()
	out := map[string]plugin{}
	for _, id := range []string{"mail", "twilio"} {
		var p plugin
		readJSON(t, filepath.Join(pkgDir, "..", id, "plugins", id, "plugin.json"), &p)
		out[id] = p
	}
	return out
}

func memberPlugin(t *testing.T, s solution, name string) plugin {
	t.Helper()
	ps := plugins(t)
	for _, m := range s.Members {
		if strings.EqualFold(m.Name, name) {
			if m.PluginID == "" {
				t.Fatalf("member %s is not a plugin member", name)
			}
			return ps[m.PluginID]
		}
	}
	t.Fatalf("no member %s", name)
	return plugin{}
}

func TestIdentity(t *testing.T) {
	s, _ := loadSolution(t)
	if s.Format != 1 || s.ID != "customer-support" || s.Name != "Customer Support" || s.Version != "1.0.0" {
		t.Fatalf("identity: format %d id %q name %q version %q", s.Format, s.ID, s.Name, s.Version)
	}
}

func TestMembers(t *testing.T) {
	s, _ := loadSolution(t)
	ps := plugins(t)
	if v := ps["mail"].Version; v != "2.2.0" {
		t.Fatalf("the published mail plugin is %s, want 2.2.0", v)
	}
	if v := ps["twilio"].Version; v != "1.4.1" {
		t.Fatalf("the published twilio plugin is %s, want 1.4.1", v)
	}
	got := map[string]string{}
	for _, m := range s.Members {
		got[m.Name] = m.Role + m.PluginID
		if m.PluginID != "" {
			p := ps[m.PluginID]
			for k := range m.Settings {
				f, ok := p.Config[k]
				if !ok {
					t.Errorf("%s sets %s, which plugin %s does not have", m.Name, k, m.PluginID)
				}
				if f.SetBy == "person" {
					t.Errorf("%s sets the person-only setting %s; ask for it under inputs.settings", m.Name, k)
				}
			}
			for field := range m.Secrets {
				if _, ok := p.Secrets[field]; !ok {
					t.Errorf("%s binds secret %s, which plugin %s does not declare", m.Name, field, m.PluginID)
				}
			}
		}
	}
	want := map[string]string{"Manager": "manager", "Mailer": "mail", "twilio": "twilio"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("members %v, want %v", got, want)
	}
	if !strings.Contains(s.Members[0].Instructions, "support-playbook") {
		t.Fatal("the Manager's instructions do not name support-playbook")
	}
	tw := s.Members[2].Secrets
	if tw["accountSid"] != "TWILIO_ACCOUNT_SID" || tw["apiKeySid"] != "TWILIO_API_KEY_SID" || tw["apiKeySecret"] != "TWILIO_API_KEY_SECRET" {
		t.Fatalf("twilio's secrets are bound as %v", tw)
	}
}

func TestTriggers(t *testing.T) {
	s, _ := loadSolution(t)
	byName := map[string]trigger{}
	for _, tr := range s.Triggers {
		byName[tr.Name] = tr
	}
	if len(byName) != 6 {
		t.Fatalf("%d triggers, want 6: %v", len(byName), byName)
	}
	sched := func(name, member, instr string, every int, runAtInstall bool) {
		tr, ok := byName[name]
		if !ok || tr.Kind != "schedule" || tr.Schedule == nil || tr.Schedule.EverySeconds != every || tr.Member != member || tr.Instruction != instr || tr.RunAtInstall != runAtInstall {
			t.Errorf("trigger %q: %+v", name, tr)
		}
		if tr.WakeManager != "onHandbackOrFailure" {
			t.Errorf("trigger %q wakes the Manager %q, want onHandbackOrFailure", name, tr.WakeManager)
		}
	}
	sched("Check mail", "Mailer", "watch", 300, true)
	sched("Poll texts", "twilio", "poll", 60, false)

	event := func(name, typ, filter string, words ...string) {
		tr, ok := byName[name]
		if !ok || tr.Kind != "event" || tr.Event == nil || tr.Event.Type != typ || tr.Event.Filter != filter || tr.Member != "Manager" {
			t.Errorf("trigger %q: %+v", name, tr)
			return
		}
		if tr.DailyTokenCap == nil || *tr.DailyTokenCap < 1 {
			t.Errorf("trigger %q has no daily token cap", name)
		}
		for _, w := range words {
			if !strings.Contains(tr.Instruction, w) {
				t.Errorf("trigger %q's instruction does not say %q: %s", name, w, tr.Instruction)
			}
		}
	}
	event("New mail", "plugin.mail.received", "", "support-playbook", "ack {event.ids}")
	event("Unanswered inquiry", "plugin.mail.unanswered", "", "support-playbook", "ONE reminder")
	event("Inquiry answered", "plugin.mail.answered", "", "support-playbook", "closed", "Send no text")
	event("Summary asked by text", "plugin.twilio.command", "command eq summary", "support-playbook", "{event.from}")

	// Every trigger that wakes the agent has a cap; plugin members run no model.
	for _, tr := range s.Triggers {
		if tr.Member == "Manager" && tr.DailyTokenCap == nil {
			t.Errorf("Manager trigger %q has no dailyTokenCap", tr.Name)
		}
		if tr.Kind != "schedule" && tr.RunAtInstall {
			t.Errorf("trigger %q sets runAtInstall on a %s trigger", tr.Name, tr.Kind)
		}
	}
}

var eventToken = regexp.MustCompile(`\{event\.([A-Za-z0-9_]+)\}`)

// Every event a trigger names is one the published plugin declares, and every {event.<field>} and
// filter field is a field of it.
func TestEventsAreThePluginsOwn(t *testing.T) {
	s, _ := loadSolution(t)
	ps := plugins(t)
	for _, tr := range s.Triggers {
		if tr.Kind != "event" {
			continue
		}
		parts := strings.SplitN(tr.Event.Type, ".", 3)
		if len(parts) != 3 || parts[0] != "plugin" {
			t.Errorf("trigger %q: %s is not a plugin event", tr.Name, tr.Event.Type)
			continue
		}
		p, ok := ps[parts[1]]
		if !ok {
			t.Errorf("trigger %q: no plugin %s in this package", tr.Name, parts[1])
			continue
		}
		var fields []string
		found := false
		for _, e := range p.Events.Publishes {
			if e.Type == parts[2] {
				found = true
				for _, f := range e.Fields {
					fields = append(fields, f.Name)
				}
			}
		}
		if !found {
			t.Errorf("trigger %q: plugin %s does not publish %s", tr.Name, p.ID, parts[2])
			continue
		}
		for _, m := range eventToken.FindAllStringSubmatch(tr.Instruction, -1) {
			if !slices.Contains(fields, m[1]) {
				t.Errorf("trigger %q uses {event.%s}, which %s does not carry (%v)", tr.Name, m[1], tr.Event.Type, fields)
			}
		}
		if tr.Event.Filter != "" {
			f := strings.Fields(tr.Event.Filter)
			if len(f) != 3 || (f[1] != "eq" && f[1] != "contains") || !slices.Contains(fields, f[0]) {
				t.Errorf("trigger %q: filter %q is not <field> eq|contains <value> on a field of %s", tr.Name, tr.Event.Filter, tr.Event.Type)
			}
		}
	}
}

func TestInputsAndSetup(t *testing.T) {
	s, raw := loadSolution(t)
	ps := plugins(t)
	asked := map[string]memberRef{}
	for _, in := range s.Inputs.Settings {
		p := memberPlugin(t, s, in.Member)
		f, ok := p.Config[in.Setting]
		if !ok || f.SetBy != "person" {
			t.Errorf("inputs.settings %s.%s is not a person-only setting of %s", in.Member, in.Setting, p.ID)
		}
		asked[in.Member+"."+in.Setting] = in
	}
	if !asked["twilio.sendAllowlist"].Required {
		t.Error("the person's cell (twilio sendAllowlist) is not a required input")
	}
	for _, k := range []string{"Mailer.moveTo", "twilio.ownerCommands", "twilio.fromNumber"} {
		if _, ok := asked[k]; !ok {
			t.Errorf("%s is not asked for", k)
		}
	}
	if len(s.Inputs.Connections) != 1 || s.Inputs.Connections[0].Member != "Mailer" || s.Inputs.Connections[0].Slot != "mailbox" || !s.Inputs.Connections[0].Required {
		t.Errorf("connections: %+v", s.Inputs.Connections)
	}
	if _, ok := ps["mail"].Connections["mailbox"]; !ok {
		t.Error("the mail plugin has no mailbox slot")
	}

	// No quiet hours: the package leaves the follow quiet settings alone.
	if strings.Contains(raw, "followQuiet") || strings.Contains(raw, "followTimeZone") {
		t.Error("solution.json names a follow quiet-hours setting; the package sets no quiet window")
	}
	// markRead stays off: nothing asks for it or sets it.
	for _, in := range append(append([]memberRef{}, s.Inputs.Settings...), s.Setup.Settings...) {
		if in.Setting == "markRead" {
			t.Error("solution.json asks for markRead; it stays off, at the plugin's default")
		}
	}
	for _, m := range s.Members {
		if _, set := m.Settings["markRead"]; set {
			t.Errorf("%s sets markRead; it stays off, at the plugin's default", m.Name)
		}
	}
	if f := ps["mail"].Config["markRead"]; string(f.Default) != "false" {
		t.Errorf("mail's markRead defaults to %s", f.Default)
	}

	// The setup wizard: limits as Yawble's check has them, every required input asked, defaults
	// the plugin takes.
	if lines := strings.Split(s.Setup.Welcome, "\n"); s.Setup.Welcome == "" || len(lines) > 3 || len(s.Setup.Welcome) > 280 {
		t.Errorf("setup.welcome is %d lines, %d characters", len(lines), len(s.Setup.Welcome))
	}
	if len(s.Setup.Needs) > 120 || strings.Contains(s.Setup.Needs, "\n") {
		t.Errorf("setup.needs: %q", s.Setup.Needs)
	}
	questions := map[string]memberRef{}
	for _, q := range append(append([]memberRef{}, s.Setup.Connections...), s.Setup.Settings...) {
		key := q.Member + "." + q.Setting + q.Slot
		if _, dup := questions[key]; dup {
			t.Errorf("setup asks %s twice", key)
		}
		questions[key] = q
		if q.Question == "" || len(q.Question) > 100 || strings.Contains(q.Question, "\n") {
			t.Errorf("setup question for %s: %q", key, q.Question)
		}
		if len(q.Hint) > 200 || strings.Contains(q.Hint, "\n") {
			t.Errorf("setup hint for %s is %d characters", key, len(q.Hint))
		}
	}
	for _, in := range s.Inputs.Settings {
		if _, ok := questions[in.Member+"."+in.Setting]; in.Required && !ok {
			t.Errorf("required input %s.%s has no setup question", in.Member, in.Setting)
		}
	}
	if _, ok := questions["Mailer.mailbox"]; !ok {
		t.Error("the mailbox connection has no setup question")
	}
	def := func(key string, want any) {
		q, ok := questions[key]
		if !ok {
			t.Errorf("setup does not ask %s", key)
			return
		}
		var got any
		if err := json.Unmarshal(q.Default, &got); err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("setup default for %s is %s, want %v", key, q.Default, want)
		}
	}
	def("Mailer.moveTo", []any{"Junk", "New Customers"})
	def("twilio.ownerCommands", true)
	def("twilio.mode", "send")
	if !slices.Contains(ps["twilio"].Config["mode"].Enum, "send") {
		t.Error("twilio's mode does not take send")
	}

	// One twilio member per Twilio number: the wizard says so where it asks for the number.
	hint := strings.ToLower(questions["twilio.fromNumber"].Hint)
	if !strings.Contains(hint, "another twilio team") || !strings.Contains(hint, "both poll") || !strings.Contains(hint, "uninstall") {
		t.Errorf("the fromNumber question does not warn about another Twilio team on the number: %q", hint)
	}

	// ownerKeywords is left at the plugin's default, which holds summary.
	var kw string
	_ = json.Unmarshal(ps["twilio"].Config["ownerKeywords"].Default, &kw)
	if !slices.Contains(strings.Split(strings.ReplaceAll(kw, " ", ""), ","), "summary") {
		t.Errorf("twilio's default ownerKeywords %q does not hold summary", kw)
	}

	for _, p := range s.Panel.Settings {
		if _, ok := memberPlugin(t, s, p.Member).Config[p.Setting]; !ok {
			t.Errorf("panel.settings %s.%s is not a setting of its plugin", p.Member, p.Setting)
		}
	}
	if s.Panel.PrimarySite != "support" || !strings.Contains(s.Panel.Status, "{data.inquiries.count status=open}") {
		t.Errorf("panel: %+v", s.Panel)
	}
}

// Every site a plugin reads ships with the package, and the mail and twilio sites are exactly the
// published packages' own.
func TestSites(t *testing.T) {
	s, _ := loadSolution(t)
	for _, name := range s.Sites {
		if _, err := os.Stat(filepath.Join(pkgDir, "sites", name, "index.html")); err != nil {
			t.Errorf("site %s: %v", name, err)
		}
	}
	for _, p := range plugins(t) {
		for _, r := range p.Reads {
			if !slices.Contains(s.Sites, r.Site) {
				t.Errorf("plugin %s reads site %s, which the package does not ship", p.ID, r.Site)
			}
		}
	}
	for _, name := range []string{"mail", "twilio"} {
		ours := filepath.Join(pkgDir, "sites", name)
		theirs := filepath.Join(pkgDir, "..", name, "sites", name)
		if a, b := tree(t, ours), tree(t, theirs); !reflect.DeepEqual(a, b) {
			t.Errorf("sites/%s differs from packages/%s/sites/%s; copy it again", name, name, name)
		}
	}
}

func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		r, _ := filepath.Rel(root, p)
		out[r] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The support page keeps to the site sandbox: no inline code or styles, no storage, no innerHTML.
func TestSupportSiteIsSafe(t *testing.T) {
	dir := filepath.Join(pkgDir, "sites", "support")
	html, _ := os.ReadFile(filepath.Join(dir, "index.html"))
	js, _ := os.ReadFile(filepath.Join(dir, "app.js"))
	if !bytes.Contains(html, []byte(`<script src="/sites/_sdk/site.js"></script>`)) || !bytes.Contains(html, []byte(`src="app.js"`)) {
		t.Error("index.html does not load the helper script and app.js")
	}
	for _, bad := range []string{"<script>", "style=", "onclick=", "onload=", "http://", "https://"} {
		if bytes.Contains(html, []byte(bad)) {
			t.Errorf("index.html holds %q", bad)
		}
	}
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "localStorage", "sessionStorage", "alert(", "confirm(", "prompt(", "fetch("} {
		if bytes.Contains(js, []byte(bad)) {
			t.Errorf("app.js uses %s", bad)
		}
	}
	for _, id := range regexp.MustCompile(`getElementById\('([a-z-]+)'\)`).FindAllStringSubmatch(string(js), -1) {
		if !bytes.Contains(html, []byte(`id="`+id[1]+`"`)) {
			t.Errorf("app.js reads #%s, which index.html does not have", id[1])
		}
	}
	for _, id := range []string{"open", "closed"} {
		for _, suffix := range []string{"", "-none", "-table"} {
			if !bytes.Contains(html, []byte(`id="`+id+suffix+`"`)) {
				t.Errorf("index.html has no #%s%s", id, suffix)
			}
		}
	}
}

// The plugins are the published ones: each build.sh builds that package's plugin and nothing else.
func TestPluginBuildsArePublishedOnes(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(pkgDir, "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		ids = append(ids, e.Name())
		dir := filepath.Join(pkgDir, "plugins", e.Name())
		files, _ := os.ReadDir(dir)
		if len(files) != 1 || files[0].Name() != "build.sh" {
			t.Errorf("plugins/%s holds more than build.sh; the source stays in packages/%s", e.Name(), e.Name())
		}
		script, err := os.ReadFile(filepath.Join(dir, "build.sh"))
		if err != nil {
			t.Fatal(err)
		}
		want := `exec sh "$here/../../../` + e.Name() + `/plugins/` + e.Name() + `/build.sh"`
		if !bytes.Contains(script, []byte(want)) {
			t.Errorf("plugins/%s/build.sh does not run the published build: want %s", e.Name(), want)
		}
		if _, err := os.Stat(filepath.Join(pkgDir, "..", e.Name(), "plugins", e.Name(), "build.sh")); err != nil {
			t.Errorf("the published build: %v", err)
		}
	}
	if !reflect.DeepEqual(ids, []string{"mail", "twilio"}) {
		t.Errorf("plugins/ holds %v, want mail and twilio", ids)
	}
}

// The playbook holds the five rules in order, with the commands the plugins take.
func TestPlaybook(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(pkgDir, "skills", "support-playbook.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	front, body, ok := strings.Cut(strings.TrimPrefix(text, "---\n"), "\n---\n")
	if !ok || !strings.HasPrefix(text, "---\n") {
		t.Fatal("support-playbook.md has no front matter")
	}
	for _, line := range []string{"name: support-playbook", "roles: manager"} {
		if !strings.Contains(front, line+"\n") && !strings.HasSuffix(front, line) {
			t.Errorf("front matter lacks %q", line)
		}
	}
	if !strings.Contains(front, "description: ") || strings.TrimSpace(body) == "" {
		t.Error("front matter lacks a description, or there is no body")
	}

	rules := []string{
		"1. **Spam.**",
		"2. **Inquiry.**",
		"3. **New customer.**",
		"4. **Order.**",
		"5. **Anything else.**",
	}
	last := -1
	for _, r := range rules {
		i := strings.Index(body, r)
		if i <= last {
			t.Fatalf("rule %q is missing or out of order", r)
		}
		last = i
	}
	rule := func(n int) string {
		start := strings.Index(body, rules[n])
		end := len(body)
		if n+1 < len(rules) {
			end = strings.Index(body, rules[n+1])
		} else if e := strings.Index(body[start:], "\n\n"); e > 0 {
			end = start + e
		}
		return body[start:end]
	}
	for n, words := range [][]string{
		{"move Junk <id>", "Never mark it read", "not counted"},
		{"`inquiry`", "any case", "alert at once", "follow <id> every:1h", "inquiries"},
		{"`New customer`", "newCustomers", `move "New Customers" <id>`},
		{"`Order`", "orders"},
		{"other"},
	} {
		for _, w := range words {
			if !strings.Contains(rule(n), w) {
				t.Errorf("rule %d does not say %q:\n%s", n+1, w, rule(n))
			}
		}
	}
	for _, w := range []string{"first rule that\nmatches wins", "site `support`", "**`counts`**", "**`inquiries`**", "ack <ids>", "untrusted", "No quiet hours", "Send no text"} {
		if !strings.Contains(body, w) {
			t.Errorf("the playbook does not say %q", w)
		}
	}
	if strings.Contains(body, "mark-read") {
		t.Error("the playbook tells Mailer to mark mail read")
	}
}

var (
	phone = regexp.MustCompile(`\+?1?\(?\d{3}\)?[ .-]?\d{3}[ .-]?\d{4}\b`)
	email = regexp.MustCompile(`[A-Za-z0-9._%+-]+@([A-Za-z0-9-]+\.)+[A-Za-z]{2,}`)
)

// The package is generic: phone numbers are 555 numbers and addresses are at reserved domains.
func TestGeneric(t *testing.T) {
	ours := map[string]bool{"sites/mail": true, "sites/twilio": true, ".tests": true}
	err := filepath.WalkDir(pkgDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(pkgDir, p)
		if d.IsDir() {
			if ours[filepath.ToSlash(r)] {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, m := range phone.FindAllString(string(data), -1) {
			digits := regexp.MustCompile(`\D`).ReplaceAllString(m, "")
			digits = strings.TrimPrefix(digits, "1")
			if len(digits) == 10 && digits[3:6] != "555" && digits[:3] != "555" {
				t.Errorf("%s: %q is not a 555 number", r, m)
			}
		}
		for _, m := range email.FindAllStringSubmatch(string(data), -1) {
			addr := strings.ToLower(m[0])
			if !regexp.MustCompile(`@([a-z0-9-]+\.)*example\.(com|net|org)$`).MatchString(addr) {
				t.Errorf("%s: %s is not at a reserved domain", r, m[0])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
