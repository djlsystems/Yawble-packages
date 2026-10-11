package customersupporttest

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
)

// The support page's view of fake counts and inquiries, run in node: the totals, the last summary,
// open inquiries oldest first and closed ones newest first. Skipped where node is not installed.
func TestSupportSiteView(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	app, err := filepath.Abs(filepath.Join(pkgDir, "sites", "support", "app.js"))
	if err != nil {
		t.Fatal(err)
	}
	script := `
const { view } = require(process.argv[1]);
const counts = [{ id: 'running', doc: { newCustomers: 2, orders: '5', other: -1, since: '2026-10-01T09:00:00Z',
  lastBatch: { at: '2026-10-11T14:05:00Z', lines: ['Order: shop@example.com - Order 1042'] } } }];
const inquiries = [
  { id: 'inq-2', doc: { messageId: '2', from: 'sam@example.net', subject: 'Inquiry: bulk', since: '2026-10-11T12:00:00Z', status: 'open' } },
  { id: 'inq-1', doc: { messageId: '1', from: 'pat@example.com', subject: '<b>Inquiry</b>', since: '2026-10-11T10:00:00Z', status: 'open', reminders: 3 } },
  { id: 'inq-0', doc: { messageId: '0', from: 'kim@example.org', subject: 'inquiry', since: '2026-10-10T10:00:00Z', status: 'closed' } },
  { id: 'inq-3', doc: { messageId: '3', from: 'lee@example.org', subject: 'inquiry 2', since: '2026-10-10T11:00:00Z', status: 'closed' } },
];
process.stdout.write(JSON.stringify({ full: view(counts, inquiries), empty: view([], []) }));
`
	out, err := exec.Command(node, "-e", script, app).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got struct {
		Full struct {
			Counts struct {
				NewCustomers, Orders, Other int
				Since                       string
			}
			Batch struct {
				At    string
				Lines []string
			}
			Open, Closed []struct{ MessageID, Subject string }
		}
		Empty struct {
			Counts any
			Batch  any
			Open   []any
		}
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	f := got.Full
	if f.Counts.NewCustomers != 2 || f.Counts.Orders != 5 || f.Counts.Other != 0 || f.Counts.Since == "" {
		t.Errorf("counts: %+v", f.Counts)
	}
	if len(f.Batch.Lines) != 1 || f.Batch.At == "" {
		t.Errorf("batch: %+v", f.Batch)
	}
	if len(f.Open) != 2 || f.Open[0].MessageID != "1" || f.Open[1].MessageID != "2" || f.Open[0].Subject != "<b>Inquiry</b>" {
		t.Errorf("open: %+v", f.Open)
	}
	if len(f.Closed) != 2 || f.Closed[0].MessageID != "3" {
		t.Errorf("closed: %+v", f.Closed)
	}
	if got.Empty.Counts != nil || got.Empty.Batch != nil || len(got.Empty.Open) != 0 {
		t.Errorf("empty: %+v", got.Empty)
	}
}
