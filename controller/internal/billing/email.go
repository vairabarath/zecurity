package billing

import (
	"bytes"
	"fmt"
	htmltemplate "html/template"
	"strings"
	texttemplate "text/template"
	"time"
	"unicode"
)

// Email kinds (invoice_emails.kind, D-68).
const (
	EmailKindIssued = "issued" // sent once when the invoice is issued
	EmailKindVoided = "voided" // sent once when an issued invoice is voided
	EmailKindResend = "resend" // a super-admin resend; content follows the invoice's status
)

// EmailInvoice is the invoice snapshot an email is built from. Everything
// comes from the invoice row and its lines (D-66 snapshot): never from the
// live tenant, plan card or settings, so an email always matches the invoice.
type EmailInvoice struct {
	ID              string
	Number          string // set for issued, paid and void invoices
	Status          string // issued | paid | void
	TenantID        string
	TenantName      string
	Month           time.Time // first day of the UTC month
	Currency        string
	PlanName        string
	Lines           []EmailLine
	SubtotalMinor   int64
	TaxRateBP       int
	TaxLabel        string
	TaxMinor        int64
	TotalMinor      int64
	IncompleteHours int
	VoidReason      string
	ProviderName    string // BILLING_PROVIDER_NAME
}

// EmailLine is one invoice line (invoice_lines).
type EmailLine struct {
	Kind           string // base | meter
	Meter          string
	Quantity       int64
	Included       int64
	Unit           int64
	BillableUnits  int64
	UnitPriceMinor int64
	AmountMinor    int64
}

// ComposeInvoiceEmail builds the subject, plain-text and HTML bodies for one
// invoice email. A void invoice (kind voided, or a resend of a void invoice)
// gets the void notice; otherwise the invoice itself. The bodies hold the
// invoice content only: no link into the provider console, no secrets.
func ComposeInvoiceEmail(kind string, inv EmailInvoice) (subject, text, html string, err error) {
	switch kind {
	case EmailKindIssued, EmailKindVoided, EmailKindResend:
	default:
		return "", "", "", fmt.Errorf("billing: unknown email kind %q", kind)
	}
	if inv.Number == "" {
		return "", "", "", fmt.Errorf("billing: invoice %s has no number (not issued)", inv.ID)
	}
	void := kind == EmailKindVoided || inv.Status == "void"
	v := newEmailView(inv, void)
	provider := v.Provider
	if void {
		subject = fmt.Sprintf("%s: invoice %s has been voided", provider, inv.Number)
	} else {
		subject = fmt.Sprintf("%s: invoice %s for %s", provider, inv.Number, v.Month)
		if inv.Status == "paid" {
			subject += " (paid)"
		}
	}
	subject = oneLine(subject, 200)
	var tb, hb bytes.Buffer
	if err := textTmpl.Execute(&tb, v); err != nil {
		return "", "", "", fmt.Errorf("billing: render email text: %w", err)
	}
	if err := htmlTmpl.Execute(&hb, v); err != nil {
		return "", "", "", fmt.Errorf("billing: render email html: %w", err)
	}
	return subject, strings.ReplaceAll(tb.String(), "\n", "\r\n"), hb.String(), nil
}

type emailView struct {
	Void            bool
	Paid            bool
	Provider        string
	Number          string
	Tenant          string
	Month           string
	Plan            string
	Lines           []emailLineView
	Subtotal        string
	TaxLine         string // "GST 18%" or "" when no tax
	Tax             string
	Total           string
	IncompleteHours int
	VoidReason      string
}

type emailLineView struct {
	Label  string
	Detail string
	Amount string
}

func newEmailView(inv EmailInvoice, void bool) emailView {
	provider := oneLine(inv.ProviderName, 80)
	if provider == "" {
		provider = "Zecurity"
	}
	v := emailView{
		Void:            void,
		Paid:            inv.Status == "paid",
		Provider:        provider,
		Number:          oneLine(inv.Number, 40),
		Tenant:          oneLine(inv.TenantName, 100),
		Month:           inv.Month.UTC().Format("January 2006"),
		Plan:            oneLine(inv.PlanName, 64),
		Subtotal:        FormatMoney(inv.SubtotalMinor, inv.Currency),
		Total:           FormatMoney(inv.TotalMinor, inv.Currency),
		IncompleteHours: inv.IncompleteHours,
		VoidReason:      oneLine(inv.VoidReason, 500),
	}
	if inv.TaxRateBP > 0 {
		label := oneLine(inv.TaxLabel, 32)
		if label == "" {
			label = "Tax"
		}
		v.TaxLine = label + " " + FormatRate(inv.TaxRateBP)
		v.Tax = FormatMoney(inv.TaxMinor, inv.Currency)
	}
	for _, l := range inv.Lines {
		lv := emailLineView{Amount: FormatMoney(l.AmountMinor, inv.Currency)}
		if l.Kind == "base" {
			lv.Label = "Monthly fee"
			if v.Plan != "" {
				lv.Detail = "Plan " + v.Plan
			}
		} else {
			unit := FormatUnit(l.Meter, l.Unit)
			lv.Label = MeterLabel(l.Meter)
			lv.Detail = fmt.Sprintf("%s used, %s included; %d × %s per %s",
				FormatQuantity(l.Meter, l.Quantity), FormatQuantity(l.Meter, l.Included),
				l.BillableUnits, FormatMoney(l.UnitPriceMinor, inv.Currency), unit)
		}
		v.Lines = append(v.Lines, lv)
	}
	return v
}

// oneLine drops control characters (incl. newlines) and caps the length, so
// tenant- or operator-entered text can't break a header or the layout.
func oneLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

var textTmpl = texttemplate.Must(texttemplate.New("text").Parse(`{{if .Void -}}
Invoice {{.Number}} from {{.Provider}} has been voided.

Workspace: {{.Tenant}}
Month: {{.Month}}
Amount: {{.Total}}
{{- if .VoidReason}}
Reason: {{.VoidReason}}{{end}}

You don't need to pay this invoice. If a corrected invoice is issued, you
will receive it separately.
{{- else -}}
Invoice {{.Number}} from {{.Provider}}{{if .Paid}} (paid){{end}}

Workspace: {{.Tenant}}
Month: {{.Month}}
{{range .Lines}}
{{.Label}}: {{.Amount}}{{if .Detail}}
  {{.Detail}}{{end}}
{{- end}}

Subtotal: {{.Subtotal}}
{{if .TaxLine}}{{.TaxLine}}: {{.Tax}}
{{else}}Tax not included.
{{end -}}
Total: {{.Total}}
{{- if .IncompleteHours}}

Note: usage for {{.IncompleteHours}} hour(s) of this month could not be
collected; usage is billed as counted.{{end}}
{{- end}}

This email was sent to the administrators of {{.Tenant}}.
`))

var htmlTmpl = htmltemplate.Must(htmltemplate.New("html").Parse(`<!doctype html>
<html><body style="font-family:Arial,Helvetica,sans-serif;color:#1a1a1a;max-width:640px;margin:0 auto;padding:16px">
{{if .Void -}}
<h2 style="margin:0 0 12px">Invoice {{.Number}} has been voided</h2>
<p>Invoice <strong>{{.Number}}</strong> from {{.Provider}} for <strong>{{.Tenant}}</strong> ({{.Month}}, {{.Total}}) has been voided.</p>
{{if .VoidReason}}<p>Reason: {{.VoidReason}}</p>{{end}}
<p>You don't need to pay this invoice. If a corrected invoice is issued, you will receive it separately.</p>
{{- else -}}
<h2 style="margin:0 0 4px">Invoice {{.Number}}{{if .Paid}} (paid){{end}}</h2>
<p style="margin:0 0 16px;color:#555">{{.Provider}} · {{.Tenant}} · {{.Month}}</p>
<table style="width:100%;border-collapse:collapse" cellpadding="6">
{{range .Lines}}<tr style="border-bottom:1px solid #ddd"><td>{{.Label}}{{if .Detail}}<br><span style="color:#666;font-size:13px">{{.Detail}}</span>{{end}}</td><td style="text-align:right;white-space:nowrap">{{.Amount}}</td></tr>
{{end}}<tr><td>Subtotal</td><td style="text-align:right;white-space:nowrap">{{.Subtotal}}</td></tr>
{{if .TaxLine}}<tr><td>{{.TaxLine}}</td><td style="text-align:right;white-space:nowrap">{{.Tax}}</td></tr>
{{else}}<tr><td colspan="2" style="color:#666;font-size:13px">Tax not included.</td></tr>
{{end}}<tr style="font-weight:bold;border-top:2px solid #1a1a1a"><td>Total</td><td style="text-align:right;white-space:nowrap">{{.Total}}</td></tr>
</table>
{{if .IncompleteHours}}<p style="color:#8a5a00">Usage for {{.IncompleteHours}} hour(s) of this month could not be collected; usage is billed as counted.</p>{{end}}
{{- end}}
<p style="color:#888;font-size:12px;margin-top:24px">This email was sent to the administrators of {{.Tenant}}.</p>
</body></html>
`))
