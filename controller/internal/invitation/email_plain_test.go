package invitation

import "testing"

// F14: SendPlain refuses multi-line recipients and subjects (header
// injection) and, without SMTP_HOST, sends nothing.
func TestSendPlain_RejectsHeaderInjectionAndHasDevMode(t *testing.T) {
	dev := NewEmailer("", "587", "", "", "")
	for _, c := range []struct{ to, subject string }{
		{"a@x\r\nBcc: evil@x", "s"},
		{"a@x", "s\nBcc: evil@x"},
		{"a@x\n", "s"},
	} {
		if err := dev.SendPlain(c.to, c.subject, "body"); err == nil {
			t.Fatalf("SendPlain(%q, %q) accepted a multi-line header", c.to, c.subject)
		}
	}
	if err := dev.SendPlain("a@x", "Zecurity: 1 certificate needs attention", "body"); err != nil {
		t.Fatalf("dev mode: %v", err)
	}
}
