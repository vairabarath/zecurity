package auditexport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// decodeStrict decodes cfg into v, rejecting unknown fields. Empty or null is
// an empty config.
func decodeStrict(cfg json.RawMessage, v any) error {
	cfg = bytes.TrimSpace(cfg)
	if len(cfg) == 0 || string(cfg) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(cfg))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid adapter config: %w", err)
	}
	return nil
}

// ── webhook (P-6) ───────────────────────────────────────────────────────────

// Webhook POSTs {"schema","workspace_id","stream","events"} as JSON, signed
// with X-Zecurity-Signature. Any 2xx acknowledges the batch.
type Webhook struct{}

func (Webhook) Kind() string { return "webhook" }

func (Webhook) ValidateConfig(cfg json.RawMessage) error {
	var c struct{}
	return decodeStrict(cfg, &c)
}

// WebhookBody is the webhook request body.
type WebhookBody struct {
	Schema      string  `json:"schema"`
	WorkspaceID string  `json:"workspace_id"`
	Stream      string  `json:"stream"`
	Events      []Event `json:"events"`
}

func (Webhook) Deliver(ctx context.Context, c *http.Client, d Destination, b Batch) error {
	body, err := json.Marshal(WebhookBody{Schema: Schema, WorkspaceID: b.WorkspaceID, Stream: b.Stream, Events: b.Events})
	if err != nil {
		return fmt.Errorf("encode batch: %w", err)
	}
	return post(ctx, c, d, b, "application/json", body, time.Now())
}

// ── Splunk HTTP Event Collector (S-8) ──────────────────────────────────────

// SplunkHEC sends events to /services/collector/event as concatenated HEC
// envelopes. The HEC token travels as "Authorization: Splunk <token>" (the
// sink's sealed auth header). The batch is also signed, harmlessly.
type SplunkHEC struct{}

func (SplunkHEC) Kind() string { return "splunk_hec" }

// SplunkHECConfig is the non-secret HEC settings.
type SplunkHECConfig struct {
	Index      string `json:"index,omitempty"`
	Source     string `json:"source,omitempty"`
	Sourcetype string `json:"sourcetype,omitempty"`
}

var hecField = regexp.MustCompile(`^[A-Za-z0-9_.:\-]{1,128}$`)

func (SplunkHEC) parse(cfg json.RawMessage) (SplunkHECConfig, error) {
	var c SplunkHECConfig
	if err := decodeStrict(cfg, &c); err != nil {
		return c, err
	}
	for name, v := range map[string]string{"index": c.Index, "source": c.Source, "sourcetype": c.Sourcetype} {
		if v != "" && !hecField.MatchString(v) {
			return c, fmt.Errorf("splunk %s must be 1-128 characters of A-Z a-z 0-9 _ . : -", name)
		}
	}
	if c.Source == "" {
		c.Source = "zecurity"
	}
	if c.Sourcetype == "" {
		c.Sourcetype = "zecurity:audit:v1"
	}
	return c, nil
}

func (h SplunkHEC) ValidateConfig(cfg json.RawMessage) error {
	_, err := h.parse(cfg)
	return err
}

type hecEnvelope struct {
	Time       float64 `json:"time"`
	Source     string  `json:"source"`
	Sourcetype string  `json:"sourcetype"`
	Index      string  `json:"index,omitempty"`
	Event      Event   `json:"event"`
}

func (h SplunkHEC) Deliver(ctx context.Context, c *http.Client, d Destination, b Batch) error {
	cfg, err := h.parse(d.Config)
	if err != nil {
		return &DeliveryError{Msg: "invalid splunk config"}
	}
	if d.AuthHeaderName == "" {
		return &DeliveryError{Msg: "splunk HEC token is not set"}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, ev := range b.Events {
		t, err := time.Parse(time.RFC3339Nano, ev.OccurredAt)
		if err != nil {
			return errors.New("event time is not RFC 3339")
		}
		if err := enc.Encode(hecEnvelope{
			Time:       float64(t.UnixMicro()) / 1e6,
			Source:     cfg.Source,
			Sourcetype: cfg.Sourcetype,
			Index:      cfg.Index,
			Event:      ev,
		}); err != nil {
			return fmt.Errorf("encode batch: %w", err)
		}
	}
	return post(ctx, c, d, b, "application/json", buf.Bytes(), time.Now())
}
