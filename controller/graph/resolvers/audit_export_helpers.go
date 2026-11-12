package resolvers

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/apperr"
	"github.com/yourorg/ztna/controller/internal/auditexport"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// PENDING-11 S-9 / P-8: the audit log page and SIEM export sinks.

const (
	auditLogDefaultLimit = 50
	auditLogMaxLimit     = 200
)

func auditTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func optAuditTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := auditTime(*t)
	return &s
}

// auditLogs reads the caller's workspace admin stream, newest first.
func (r *Resolver) auditLogs(ctx context.Context, before, limit *int, actionPrefix *string) (*graph.AuditLogPage, error) {
	tc := tenant.MustGet(ctx)
	n := auditLogDefaultLimit
	if limit != nil {
		n = *limit
	}
	if n < 1 || n > auditLogMaxLimit {
		return nil, apperr.UserErrorf("auditLogs: limit must be 1-%d", auditLogMaxLimit)
	}
	var beforeSeq any
	if before != nil {
		beforeSeq = int64(*before)
	}
	prefix := ""
	if actionPrefix != nil {
		prefix = *actionPrefix
	}
	rows, err := r.Pool.Query(ctx,
		`SELECT seq, id::text, action, actor_user_id::text, actor_email, target_type, target_id,
		        details::text, created_at, row_hash
		   FROM audit_logs
		  WHERE tenant_id = $1
		    AND ($2::bigint IS NULL OR seq < $2)
		    AND starts_with(action, $3)
		  ORDER BY seq DESC
		  LIMIT $4`,
		tc.TenantID, beforeSeq, prefix, n+1)
	if err != nil {
		return nil, fmt.Errorf("auditLogs: %w", err)
	}
	defer rows.Close()
	page := &graph.AuditLogPage{Entries: []*graph.AuditLogEntry{}}
	for rows.Next() {
		var (
			e       graph.AuditLogEntry
			seq     int64
			created time.Time
			hash    []byte
		)
		if err := rows.Scan(&seq, &e.ID, &e.Action, &e.ActorUserID, &e.ActorEmail, &e.TargetType, &e.TargetID,
			&e.Details, &created, &hash); err != nil {
			return nil, fmt.Errorf("auditLogs: %w", err)
		}
		e.Seq = int(seq)
		e.CreatedAt = auditTime(created)
		e.RowHash = hex.EncodeToString(hash)
		page.Entries = append(page.Entries, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auditLogs: %w", err)
	}
	if len(page.Entries) > n {
		page.Entries = page.Entries[:n]
		next := page.Entries[n-1].Seq
		page.NextBefore = &next
	}
	return page, nil
}

func (r *Resolver) auditExportStore() (*auditexport.Store, error) {
	if r.AuditExport == nil {
		return nil, errors.New("audit export is not configured")
	}
	return r.AuditExport, nil
}

// sinkError maps store errors to caller-safe messages.
func sinkError(op string, err error) error {
	var ie *auditexport.InputError
	switch {
	case errors.As(err, &ie):
		return apperr.UserErrorf("%s: %s", op, ie.Msg)
	case errors.Is(err, auditexport.ErrNotFound):
		return apperr.UserErrorf("%s: audit export sink not found", op)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func sinkKind(k graph.AuditExportSinkKind) (string, error) {
	switch k {
	case graph.AuditExportSinkKindWebhook:
		return "webhook", nil
	case graph.AuditExportSinkKindSplunkHec:
		return "splunk_hec", nil
	case graph.AuditExportSinkKindS3:
		return "s3", nil
	}
	return "", apperr.UserErrorf("unknown sink kind %q", k)
}

func streamNames(in []graph.AuditStream) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.ToLower(string(s)))
	}
	return out
}

// splunkSettings turns the Splunk input into the adapter config and the
// sealed "Authorization: Splunk <token>" header (token optional on update).
func splunkSettings(in *graph.SplunkHecInput) (json.RawMessage, *auditexport.AuthHeader, error) {
	cfg := auditexport.SplunkHECConfig{}
	if in.Index != nil {
		cfg.Index = *in.Index
	}
	if in.Source != nil {
		cfg.Source = *in.Source
	}
	if in.Sourcetype != nil {
		cfg.Sourcetype = *in.Sourcetype
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, nil, err
	}
	if in.Token == nil {
		return raw, nil, nil
	}
	tok := *in.Token
	if tok == "" || len(tok) > 256 || strings.ContainsAny(tok, " \t\r\n") {
		return nil, nil, apperr.UserErrorf("splunk token must be 1-256 characters without spaces")
	}
	return raw, &auditexport.AuthHeader{Name: "Authorization", Value: "Splunk " + tok}, nil
}

// s3Settings turns the S3 input into the adapter config and the sealed
// secret access key (optional on update).
func s3Settings(in *graph.S3Input) (json.RawMessage, *auditexport.AuthHeader, error) {
	opt := func(p *string) string {
		if p == nil {
			return ""
		}
		return strings.TrimSpace(*p)
	}
	raw, err := json.Marshal(auditexport.S3Config{
		Bucket:      strings.TrimSpace(in.Bucket),
		Region:      strings.TrimSpace(in.Region),
		Prefix:      opt(in.Prefix),
		Endpoint:    opt(in.Endpoint),
		AccessKeyID: strings.TrimSpace(in.AccessKeyID),
	})
	if err != nil {
		return nil, nil, err
	}
	if in.SecretAccessKey == nil {
		return raw, nil, nil
	}
	sec := *in.SecretAccessKey
	if sec == "" || len(sec) > 256 || strings.ContainsAny(sec, " \t\r\n") {
		return nil, nil, apperr.UserErrorf("S3 secret access key must be 1-256 characters without spaces")
	}
	return raw, &auditexport.AuthHeader{Name: auditexport.S3SecretSlot, Value: sec}, nil
}

func authHeaderIn(in *graph.AuditExportAuthHeaderInput) *auditexport.AuthHeader {
	if in == nil {
		return nil
	}
	return &auditexport.AuthHeader{Name: in.Name, Value: in.Value}
}

func changedBy(ctx context.Context) auditexport.ChangedBy {
	tc := tenant.MustGet(ctx)
	return auditexport.ChangedBy{UserID: tc.UserID, Email: tc.Email}
}

func (r *Resolver) createAuditExportSink(ctx context.Context, in graph.CreateAuditExportSinkInput) (*graph.AuditExportSinkWithSecret, error) {
	store, err := r.auditExportStore()
	if err != nil {
		return nil, err
	}
	kind, err := sinkKind(in.Kind)
	if err != nil {
		return nil, err
	}
	ci := auditexport.CreateInput{
		WorkspaceID: tenant.MustGet(ctx).TenantID,
		Name:        in.Name,
		Kind:        kind,
		URL:         derefString(in.URL),
		Streams:     streamNames(in.Streams),
		Enabled:     in.Enabled == nil || *in.Enabled,
		StartFrom:   auditexport.StartAllHistory,
		AuthHeader:  authHeaderIn(in.AuthHeader),
		Actor:       changedBy(ctx),
	}
	if in.StartFrom != nil && *in.StartFrom == graph.AuditExportStartFromNow {
		ci.StartFrom = auditexport.StartNow
	}
	switch kind {
	case "splunk_hec":
		if in.AuthHeader != nil {
			return nil, apperr.UserErrorf("createAuditExportSink: a Splunk HEC sink takes its token in splunk.token, not authHeader")
		}
		if in.Splunk == nil || in.Splunk.Token == nil {
			return nil, apperr.UserErrorf("createAuditExportSink: a Splunk HEC sink needs splunk.token")
		}
		cfg, hdr, err := splunkSettings(in.Splunk)
		if err != nil {
			return nil, err
		}
		ci.Config, ci.AuthHeader = cfg, hdr
	case "s3":
		if in.AuthHeader != nil || in.Splunk != nil || in.URL != nil {
			return nil, apperr.UserErrorf("createAuditExportSink: an S3 sink takes only s3 settings (no url, authHeader or splunk)")
		}
		if in.S3 == nil || in.S3.SecretAccessKey == nil {
			return nil, apperr.UserErrorf("createAuditExportSink: an S3 sink needs s3 settings with secretAccessKey")
		}
		cfg, hdr, err := s3Settings(in.S3)
		if err != nil {
			return nil, err
		}
		ci.Config, ci.AuthHeader = cfg, hdr
	default:
		if in.Splunk != nil || in.S3 != nil {
			return nil, apperr.UserErrorf("createAuditExportSink: splunk and s3 settings apply to their own sink kinds only")
		}
		if in.URL == nil {
			return nil, apperr.UserErrorf("createAuditExportSink: a webhook sink needs url")
		}
	}
	sink, secret, err := store.Create(ctx, ci)
	if err != nil {
		return nil, sinkError("createAuditExportSink", err)
	}
	return &graph.AuditExportSinkWithSecret{Sink: toGraphSink(sink), SigningSecret: secret}, nil
}

func (r *Resolver) updateAuditExportSink(ctx context.Context, id string, in graph.UpdateAuditExportSinkInput) (*graph.AuditExportSink, error) {
	store, err := r.auditExportStore()
	if err != nil {
		return nil, err
	}
	ws := tenant.MustGet(ctx).TenantID
	cur, err := store.Get(ctx, ws, id)
	if err != nil {
		return nil, sinkError("updateAuditExportSink", err)
	}
	ui := auditexport.UpdateInput{
		WorkspaceID: ws,
		ID:          id,
		Name:        in.Name,
		URL:         in.URL,
		Enabled:     in.Enabled,
		AuthHeader:  authHeaderIn(in.AuthHeader),
		ClearAuth:   in.ClearAuthHeader != nil && *in.ClearAuthHeader,
		Actor:       changedBy(ctx),
	}
	if in.Streams != nil {
		ui.Streams = streamNames(in.Streams)
	}
	if in.S3 != nil && cur.Kind != "s3" {
		return nil, apperr.UserErrorf("updateAuditExportSink: s3 settings apply to S3 sinks only")
	}
	if cur.Kind == "s3" {
		if in.AuthHeader != nil || in.Splunk != nil || in.URL != nil || ui.ClearAuth {
			return nil, apperr.UserErrorf("updateAuditExportSink: an S3 sink takes only s3 settings (no url, authHeader or splunk)")
		}
		if in.S3 != nil {
			cfg, hdr, err := s3Settings(in.S3)
			if err != nil {
				return nil, err
			}
			ui.Config, ui.AuthHeader = cfg, hdr
		}
	}
	if cur.Kind == "splunk_hec" {
		if in.AuthHeader != nil {
			return nil, apperr.UserErrorf("updateAuditExportSink: a Splunk HEC sink takes its token in splunk.token, not authHeader")
		}
		if in.Splunk != nil {
			cfg, hdr, err := splunkSettings(in.Splunk)
			if err != nil {
				return nil, err
			}
			ui.Config, ui.AuthHeader = cfg, hdr
		}
	} else if in.Splunk != nil {
		return nil, apperr.UserErrorf("updateAuditExportSink: splunk settings apply to Splunk HEC sinks only")
	}
	sink, err := store.Update(ctx, ui)
	if err != nil {
		return nil, sinkError("updateAuditExportSink", err)
	}
	return toGraphSink(sink), nil
}

func (r *Resolver) rotateAuditExportSinkSecret(ctx context.Context, id string) (*graph.AuditExportSinkWithSecret, error) {
	store, err := r.auditExportStore()
	if err != nil {
		return nil, err
	}
	sink, secret, err := store.RotateSecret(ctx, tenant.MustGet(ctx).TenantID, id, changedBy(ctx))
	if err != nil {
		return nil, sinkError("rotateAuditExportSinkSecret", err)
	}
	return &graph.AuditExportSinkWithSecret{Sink: toGraphSink(sink), SigningSecret: secret}, nil
}

func (r *Resolver) deleteAuditExportSink(ctx context.Context, id string) (bool, error) {
	store, err := r.auditExportStore()
	if err != nil {
		return false, err
	}
	if err := store.Delete(ctx, tenant.MustGet(ctx).TenantID, id, changedBy(ctx)); err != nil {
		return false, sinkError("deleteAuditExportSink", err)
	}
	return true, nil
}

func (r *Resolver) testAuditExportSink(ctx context.Context, id string) (*graph.AuditExportTestResult, error) {
	if r.AuditExporter == nil {
		return nil, errors.New("audit export is not configured")
	}
	msg, err := r.AuditExporter.SendTest(ctx, tenant.MustGet(ctx).TenantID, id)
	if err != nil {
		return nil, sinkError("testAuditExportSink", err)
	}
	res := &graph.AuditExportTestResult{Ok: msg == ""}
	if msg != "" {
		res.Error = &msg
	}
	return res, nil
}

func toGraphSink(s auditexport.Sink) *graph.AuditExportSink {
	g := &graph.AuditExportSink{
		ID:             s.ID,
		Name:           s.Name,
		URL:            s.URL,
		Enabled:        s.Enabled,
		AuthHeaderName: s.AuthHeaderName,
		CreatedAt:      auditTime(s.CreatedAt),
		UpdatedAt:      auditTime(s.UpdatedAt),
		Status:         []*graph.AuditExportStreamStatus{},
	}
	switch s.Kind {
	case "splunk_hec":
		g.Kind = graph.AuditExportSinkKindSplunkHec
		var cfg auditexport.SplunkHECConfig
		if json.Unmarshal(s.Config, &cfg) == nil {
			opt := func(v string) *string {
				if v == "" {
					return nil
				}
				return &v
			}
			g.SplunkIndex, g.SplunkSource, g.SplunkSourcetype = opt(cfg.Index), opt(cfg.Source), opt(cfg.Sourcetype)
		}
	case "s3":
		g.Kind = graph.AuditExportSinkKindS3
		g.AuthHeaderName = nil // the secret's storage slot is not a header
		var cfg auditexport.S3Config
		if json.Unmarshal(s.Config, &cfg) == nil {
			opt := func(v string) *string {
				if v == "" {
					return nil
				}
				return &v
			}
			g.S3Bucket, g.S3Region, g.S3Prefix = opt(cfg.Bucket), opt(cfg.Region), opt(cfg.Prefix)
			g.S3Endpoint, g.S3AccessKeyID = opt(cfg.Endpoint), opt(cfg.AccessKeyID)
		}
	default:
		g.Kind = graph.AuditExportSinkKindWebhook
	}
	for _, st := range s.Streams {
		g.Streams = append(g.Streams, graph.AuditStream(strings.ToUpper(st)))
	}
	for _, st := range s.Status {
		g.Status = append(g.Status, &graph.AuditExportStreamStatus{
			Stream:          graph.AuditStream(strings.ToUpper(st.Stream)),
			CursorSeq:       int(st.CursorSeq),
			HeadSeq:         int(st.HeadSeq),
			Lag:             int(st.Lag()),
			LastSuccessAt:   optAuditTime(st.LastSuccessAt),
			LastError:       st.LastError,
			FailingSince:    optAuditTime(st.FailingSince),
			OldestPendingAt: optAuditTime(st.OldestPendingAt),
			Lagging:         st.Lagging,
		})
	}
	return g
}

func (r *Resolver) auditExportSinks(ctx context.Context) ([]*graph.AuditExportSink, error) {
	store, err := r.auditExportStore()
	if err != nil {
		return nil, err
	}
	sinks, err := store.List(ctx, tenant.MustGet(ctx).TenantID)
	if err != nil {
		return nil, fmt.Errorf("auditExportSinks: %w", err)
	}
	out := make([]*graph.AuditExportSink, 0, len(sinks))
	for _, s := range sinks {
		out = append(out, toGraphSink(s))
	}
	return out, nil
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
