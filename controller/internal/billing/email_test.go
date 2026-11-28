package billing

import (
	"strings"
	"testing"
	"time"
)

func sampleInvoice() EmailInvoice {
	return EmailInvoice{
		ID: "inv-1", Number: "INV-2026-000001", Status: "issued", TenantID: "t-1",
		TenantName: "Acme", Month: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		Currency: "INR", PlanName: "Team", ProviderName: "Example Networks",
		Lines: []EmailLine{
			{Kind: "base", AmountMinor: 100000},
			{Kind: "meter", Meter: "relay_bytes_to_clients", Quantity: 60_500_000_000, Included: 50_000_000_000,
				Unit: 1_000_000_000, BillableUnits: 11, UnitPriceMinor: 800, AmountMinor: 8800},
		},
		SubtotalMinor: 108800, TaxRateBP: 1800, TaxLabel: "GST", TaxMinor: 19584, TotalMinor: 128384,
	}
}

func TestComposeInvoiceEmail_Issued(t *testing.T) {
	subj, text, html, err := ComposeInvoiceEmail(EmailKindIssued, sampleInvoice())
	if err != nil {
		t.Fatal(err)
	}
	if subj != "Example Networks: invoice INV-2026-000001 for November 2026" {
		t.Errorf("subject %q", subj)
	}
	for _, want := range []string{
		"Invoice INV-2026-000001 from Example Networks", "Workspace: Acme", "Month: November 2026",
		"Monthly fee: INR 1,000.00", "Plan Team",
		"Relay traffic to clients: INR 88.00", "60.50 GB used, 50.00 GB included; 11 × INR 8.00 per GB",
		"Subtotal: INR 1,088.00", "GST 18%: INR 195.84", "Total: INR 1,283.84",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Tax not included") || strings.Contains(text, "could not be") {
		t.Errorf("text has a no-tax or incomplete note it shouldn't:\n%s", text)
	}
	if !strings.Contains(text, "\r\n") || strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") {
		t.Error("text body must use CRLF line endings only")
	}
	for _, want := range []string{"INV-2026-000001", "GST 18%", "INR 1,283.84", "60.50 GB used"} {
		if !strings.Contains(html, want) {
			t.Errorf("html lacks %q", want)
		}
	}
	for _, body := range []string{text, html} {
		if strings.Contains(body, "http") || strings.Contains(body, "/provider") {
			t.Errorf("email must not link into the console:\n%s", body)
		}
	}
}

func TestComposeInvoiceEmail_NoTaxIncompletePaid(t *testing.T) {
	inv := sampleInvoice()
	inv.TaxRateBP, inv.TaxMinor, inv.TotalMinor = 0, 0, inv.SubtotalMinor
	inv.IncompleteHours = 2
	inv.Status = "paid"
	subj, text, html, err := ComposeInvoiceEmail(EmailKindResend, inv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(subj, "(paid)") {
		t.Errorf("subject %q", subj)
	}
	for _, body := range []string{text, html} {
		if !strings.Contains(body, "Tax not included.") || !strings.Contains(body, "2 hour(s)") {
			t.Errorf("body lacks the no-tax or incomplete-hours note:\n%s", body)
		}
	}
}

func TestComposeInvoiceEmail_Void(t *testing.T) {
	inv := sampleInvoice()
	inv.Status = "void"
	inv.VoidReason = "Wrong plan"
	for _, kind := range []string{EmailKindVoided, EmailKindResend} {
		subj, text, html, err := ComposeInvoiceEmail(kind, inv)
		if err != nil {
			t.Fatal(err)
		}
		if subj != "Example Networks: invoice INV-2026-000001 has been voided" {
			t.Errorf("%s subject %q", kind, subj)
		}
		for _, body := range []string{text, html} {
			if !strings.Contains(body, "Wrong plan") || strings.Contains(body, "Subtotal") {
				t.Errorf("%s: void notice wrong:\n%s", kind, body)
			}
		}
	}
}

// Tenant- and operator-entered text is escaped in HTML and kept on one line
// in the subject (no header injection, no markup).
func TestComposeInvoiceEmail_EscapesAndOneLine(t *testing.T) {
	inv := sampleInvoice()
	inv.TenantName = `<script>alert(1)</script>`
	inv.ProviderName = "Evil\r\nBcc: x@y"
	subj, _, html, err := ComposeInvoiceEmail(EmailKindIssued, inv)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(subj, "\r\n") {
		t.Errorf("subject has a line break: %q", subj)
	}
	if strings.Contains(html, "<script>") || !strings.Contains(html, "&lt;script&gt;") {
		t.Error("tenant name not escaped in HTML")
	}
}

func TestComposeInvoiceEmail_Refusals(t *testing.T) {
	inv := sampleInvoice()
	if _, _, _, err := ComposeInvoiceEmail("other", inv); err == nil {
		t.Error("unknown kind accepted")
	}
	inv.Number = ""
	if _, _, _, err := ComposeInvoiceEmail(EmailKindIssued, inv); err == nil {
		t.Error("an invoice without a number (draft) accepted")
	}
}
