package invitation

import (
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

// Sprint 26 D-68: SendMultipart refuses multi-line headers and, without
// SMTP_HOST, sends nothing.
func TestSendMultipart_RejectsHeaderInjectionAndHasDevMode(t *testing.T) {
	dev := NewEmailer("", "587", "", "", "")
	for _, c := range []struct{ to, subject string }{
		{"a@x\r\nBcc: evil@x", "s"},
		{"a@x", "s\nBcc: evil@x"},
	} {
		if err := dev.SendMultipart(c.to, c.subject, "t", "<p>h</p>"); err == nil {
			t.Fatalf("SendMultipart(%q, %q) accepted a multi-line header", c.to, c.subject)
		}
	}
	if err := dev.SendMultipart("a@x", "Invoice INV-2026-000001", "t", "<p>h</p>"); err != nil {
		t.Fatalf("dev mode: %v", err)
	}
}

// The message parses as multipart/alternative with a text and an HTML part
// whose decoded bodies are the inputs (UTF-8, long lines, encoded subject).
func TestBuildMultipart_ParsesBack(t *testing.T) {
	text := "Total: ₹1,180.00\r\n" + strings.Repeat("x", 200)
	html := "<p>Total: ₹1,180.00</p>"
	raw, err := buildMultipart("billing@p.test", "admin@t.test", "Invoice INV-2026-000001 — Acmé", text, html, "b0undary")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	subj, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || subj != "Invoice INV-2026-000001 — Acmé" {
		t.Fatalf("subject %q (%v)", subj, err)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content type %q (%v)", mt, err)
	}
	r := multipart.NewReader(msg.Body, params["boundary"])
	want := []struct{ ctype, body string }{{"text/plain", text}, {"text/html", html}}
	for i, w := range want {
		p, err := r.NextPart() // decodes quoted-printable
		if err != nil {
			t.Fatalf("part %d: %v", i, err)
		}
		ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		got, _ := io.ReadAll(p)
		if ct != w.ctype || string(got) != w.body {
			t.Fatalf("part %d = %s %q, want %s %q", i, ct, got, w.ctype, w.body)
		}
	}
	if _, err := r.NextPart(); err != io.EOF {
		t.Fatalf("extra part: %v", err)
	}
}
