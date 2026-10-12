package customersupporttest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Independent checks of Customer Support 1.0.1 (B005H), by the tester: the summary's text status
// as the playbook and the New mail trigger state it, markup in twilio's sentence on the page, and
// what the 1.0.0 migration rule does when the first event after the update is from another mailbox.

// The playbook writes lastBatch as notSent with the summary, and puts it back once twilio answers.
func TestVerifyPlaybookTextStatusFlow(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(pkgDir, "skills", "support-playbook.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(data)), " ")
	step3 := strings.Index(text, "3. Put back `running`")
	step4 := strings.Index(text, "4. Text one short summary")
	step5 := strings.Index(text, "5. Tell Mailer `ack")
	if step3 < 0 || step4 < step3 || step5 < step4 {
		t.Fatalf("New mail steps 3-5 not found in order: %d %d %d", step3, step4, step5)
	}
	if !strings.Contains(text[step3:step4], "`lastBatch` with its lines and `textStatus` `notSent`") {
		t.Errorf("step 3 does not write lastBatch as notSent: %q", text[step3:step4])
	}
	for _, w := range []string{
		"When twilio answers, put back `running` with `lastBatch.textStatus` `sent` and `textAt` now",
		"`failed`, `textAt` now and twilio's answer word for word in `textError`",
		"After an `allowlist` retry, record the answer to the retry",
	} {
		if !strings.Contains(text[step4:step5], w) {
			t.Errorf("step 4 does not say %q", w)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(pkgDir, "solution.json"))
	if !strings.Contains(string(raw), "when twilio answers, record on the support site whether that text was sent or failed") {
		t.Error("the New mail trigger instruction does not ask for the text status to be recorded")
	}
}

// twilio's sentence and summary lines with markup reach the page as text, in every status.
func TestVerifySiteMarkupInTwilioSentence(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	app, _ := filepath.Abs(filepath.Join(pkgDir, "sites", "support", "app.js"))
	script := `
const el = () => {
  const e = { textContent: '', children: [], classList: { toggle() {} },
    replaceChildren(...c) { this.children = c; }, append(...c) { this.children.push(...c); } };
  for (const p of ['innerHTML', 'outerHTML']) Object.defineProperty(e, p, { set() { throw new Error(p + ' set'); } });
  e.insertAdjacentHTML = () => { throw new Error('insertAdjacentHTML'); };
  return e;
};
const els = {};
global.document = { getElementById: (id) => (els[id] = els[id] || el()), createElement: el };
const { view, render } = require(process.argv[1]);
const at = '2026-10-11T14:05:00Z';
const run = (b) => { render(view([{ id: 'running', doc: { lastBatch: b } }], [])); return els['batch-at'].textContent; };
process.stdout.write(JSON.stringify({
  failed: run({ at, lines: [], textStatus: 'failed', textAt: at, textError: 'Refused <img src=x onerror=alert(1)><script>alert(2)</script>' }),
  noReason: run({ at, lines: [], textStatus: 'failed', textAt: at }),
  unknown: run({ at, lines: [], textStatus: 'Sent' }),
  when: new Date(at).toLocaleString(),
}));
`
	out, err := exec.Command(node, "-e", script, app).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v: %s", err, out)
	}
	var got struct{ Failed, NoReason, Unknown, When string }
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if want := "Text failed " + got.When + ": Refused <img src=x onerror=alert(1)><script>alert(2)</script>"; got.Failed != want {
		t.Errorf("failed: %q, want %q", got.Failed, want)
	}
	if want := "Text failed " + got.When + ": twilio gave no reason"; got.NoReason != want {
		t.Errorf("failed without a sentence: %q", got.NoReason)
	}
	// A status the page does not know never claims the text went out.
	if strings.HasPrefix(got.Unknown, "Texted") {
		t.Errorf("unknown status reads %q", got.Unknown)
	}
}
