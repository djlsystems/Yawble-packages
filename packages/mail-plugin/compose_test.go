package main

import (
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
)

// messageParts parses a raw message and returns its text/plain and text/html parts decoded ("" for
// a part it does not have). It fails on a base64 text part: text is never sent that way.
func messageParts(t *testing.T, raw string) (text, html string) {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("not a message: %v\n%s", err, raw)
	}
	kind, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("bad Content-Type %q", m.Header.Get("Content-Type"))
	}
	decode := func(cte string, r io.Reader) string {
		switch strings.ToLower(cte) {
		case "7bit", "8bit", "":
		case "quoted-printable":
			r = quotedprintable.NewReader(r)
		default:
			t.Fatalf("text sent as %s", cte)
		}
		b, _ := io.ReadAll(r)
		return string(b)
	}
	if kind == "text/plain" {
		return decode(m.Header.Get("Content-Transfer-Encoding"), m.Body), ""
	}
	if kind != "multipart/alternative" {
		t.Fatalf("want text/plain or multipart/alternative, got %s", kind)
	}
	r := multipart.NewReader(m.Body, params["boundary"])
	for {
		p, err := r.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("bad part: %v", err)
		}
		pk, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		body := decode(p.Header.Get("Content-Transfer-Encoding"), p)
		switch pk {
		case "text/plain":
			text = body
		case "text/html":
			html = body
		}
	}
	return text, html
}

func TestTheDefaultIsTextAndHtmlWithTheTextAsWrittenAndNoBase64(t *testing.T) {
	f := sendFake(t)
	instr := "send\nTo: person@example.com\nSubject: Weekly\n\n## Weekly note\n\nThis is **important**.\n\n- one\n- two\n\nSee https://example.com/a"
	o := runIt(t, instr, opts{allowlist: []string{"@example.com"}})
	mustOK(t, o)
	raw := rawOf(t, f.called("POST", "/gmail/v1/users/me/drafts")[0].Body, true)
	contains(t, raw, "Content-Type: multipart/alternative", "Content-Transfer-Encoding: 7bit")
	if strings.Contains(raw, "base64") {
		t.Fatalf("no part is base64:\n%s", raw)
	}
	text, html := messageParts(t, raw)
	contains(t, text, "## Weekly note", "This is **important**.", "- one")
	contains(t, html, "<h2>Weekly note</h2>", "<strong>important</strong>", "<li>one</li>", `<a href="https://example.com/a">`)
}

func TestHtmlInTheBodyIsShownAsTextAndAJavascriptLinkIsDropped(t *testing.T) {
	html := renderHTML("Hi <img src=\"https://tracker.example/p.gif\"> and <script>alert(1)</script>\n\n[click](javascript:alert(1))")
	for _, bad := range []string{"<img", "<script", "javascript:"} {
		if strings.Contains(html, bad) {
			t.Fatalf("%s reached the HTML part:\n%s", bad, html)
		}
	}
	contains(t, html, "click")
}

func TestTextOutsideAsciiIsQuotedPrintableAndDecodesToWhatWasWritten(t *testing.T) {
	raw := buildMessage(outgoing{From: "a@example.com", To: []string{"b@example.com"}, Subject: "Café", Body: "Café plans: déjà vu\nSecond line"}, true)
	contains(t, raw, "Content-Transfer-Encoding: quoted-printable")
	text, _ := messageParts(t, raw)
	if text != "Café plans: déjà vu\r\nSecond line\r\n" {
		t.Fatalf("decoded text is %q", text)
	}
}

func TestFormatTextSendsOnePlainTextPart(t *testing.T) {
	f := sendFake(t)
	instr := "send\nTo: person@example.com\nSubject: Hi\n\nThis is **plain**."
	o := runIt(t, instr, opts{allowlist: []string{"@example.com"}, config: map[string]any{"format": "text"}})
	mustOK(t, o)
	raw := rawOf(t, f.called("POST", "/gmail/v1/users/me/drafts")[0].Body, true)
	contains(t, raw, "Content-Type: text/plain; charset=\"UTF-8\"", "Content-Transfer-Encoding: 7bit")
	if strings.Contains(raw, "multipart") || strings.Contains(raw, "text/html") {
		t.Fatalf("format text sends no HTML:\n%s", raw)
	}
	text, _ := messageParts(t, raw)
	contains(t, text, "This is **plain**.")
}
