package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

// markdown turns a body into HTML. SAFE MODE, deliberately: goldmark without html.WithUnsafe
// leaves raw HTML out and drops javascript:, vbscript: and file: links. Whoever writes the body may
// have just read untrusted mail, and a tag passed through could carry a hidden link or a tracking
// image. Hard wraps keep a body written as plain lines reading as it was written.
var markdown = goldmark.New(
	goldmark.WithExtensions(extension.Linkify, extension.Strikethrough, extension.Table),
	goldmark.WithRendererOptions(gmhtml.WithHardWraps()),
)

// renderHTML is the HTML part made from a body: its Markdown rendered, in a minimal page.
func renderHTML(body string) string {
	var out bytes.Buffer
	if err := markdown.Convert([]byte(body), &out); err != nil {
		return ""
	}
	return "<!DOCTYPE html>\n<html><head><meta charset=\"utf-8\"></head>\n" +
		"<body style=\"font-family:-apple-system,'Segoe UI',Helvetica,Arial,sans-serif;font-size:15px;line-height:1.5;color:#1f1f1f\">\n" +
		out.String() + "</body></html>\n"
}

// buildMessage writes one RFC 5322 message: plain text alone, or (o.HTML set) plain text and HTML
// as the two parts of multipart/alternative, so a reader that shows no HTML still has the text.
// forSMTP leaves out the Bcc line, which a message handed to SMTP must not carry; the Gmail API and
// a saved draft keep it.
//
// TEXT IS NOT BASE64. A part is 7bit when it is ASCII with no line over 998 characters, else
// quoted-printable: base64 for plain text is valid but spam filters score it ("text disguised using
// base64"), and mail programs use it only for text that needs it.
func buildMessage(o outgoing, forSMTP bool) string {
	var b strings.Builder
	if o.From != "" {
		b.WriteString("From: " + o.From + "\r\n")
	}
	b.WriteString("To: " + strings.Join(o.To, ", ") + "\r\n")
	if len(o.Cc) > 0 {
		b.WriteString("Cc: " + strings.Join(o.Cc, ", ") + "\r\n")
	}
	if len(o.Bcc) > 0 && !forSMTP {
		b.WriteString("Bcc: " + strings.Join(o.Bcc, ", ") + "\r\n")
	}
	b.WriteString("Subject: " + mime.QEncoding.Encode("UTF-8", o.Subject) + "\r\n")
	if o.From != "" {
		now := time.Now()
		b.WriteString("Date: " + now.Format(time.RFC1123Z) + "\r\n")
		domain := o.From[strings.LastIndex(o.From, "@")+1:]
		b.WriteString(fmt.Sprintf("Message-ID: <%d.mail-plugin@%s>\r\n", now.UnixNano(), domain))
	}
	if o.InReplyTo != "" {
		b.WriteString("In-Reply-To: " + o.InReplyTo + "\r\n")
	}
	if o.References != "" {
		b.WriteString("References: " + o.References + "\r\n")
	}
	b.WriteString("MIME-Version: 1.0\r\n")

	if o.HTML == "" {
		writePart(&b, "text/plain", o.Body)
		return b.String()
	}

	boundary := newBoundary()
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n\r\n")
	b.WriteString("--" + boundary + "\r\n")
	writePart(&b, "text/plain", o.Body)
	b.WriteString("--" + boundary + "\r\n")
	writePart(&b, "text/html", o.HTML)
	b.WriteString("--" + boundary + "--\r\n")
	return b.String()
}

// writePart writes one part's Content-Type and Content-Transfer-Encoding, a blank line, then the
// text encoded, ending in CRLF.
func writePart(b *strings.Builder, kind, text string) {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	b.WriteString("Content-Type: " + kind + "; charset=\"UTF-8\"\r\n")
	if plain7bit(text) {
		b.WriteString("Content-Transfer-Encoding: 7bit\r\n\r\n")
		b.WriteString(strings.ReplaceAll(text, "\n", "\r\n"))
		if !strings.HasSuffix(text, "\n") {
			b.WriteString("\r\n")
		}
		return
	}
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	var enc bytes.Buffer
	w := quotedprintable.NewWriter(&enc)
	w.Write([]byte(strings.ReplaceAll(text, "\n", "\r\n")))
	w.Close()
	b.WriteString(enc.String())
	if !strings.HasSuffix(enc.String(), "\r\n") {
		b.WriteString("\r\n")
	}
}

// plain7bit: ASCII only, no line over 998 characters (RFC 5322's limit), so it goes as written.
func plain7bit(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if len(line) > 998 {
			return false
		}
	}
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 || (text[i] < 0x20 && text[i] != '\n' && text[i] != '\t') {
			return false
		}
	}
	return true
}

func newBoundary() string {
	var r [12]byte
	rand.Read(r[:])
	return "mail-plugin-" + hex.EncodeToString(r[:])
}
