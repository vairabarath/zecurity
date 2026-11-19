package tenantexport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/audit"
)

// Handler serves a suspended tenant's own log download during the grace
// (2026-11-18 design). The token travels in the request body, never the URL;
// the admin UI page reads it from the URL fragment (emailed link) or from the
// export-only sign-in redirect.
//
//	POST /api/tenant-export/info     {token} → {workspace_name, slug, delete_after}
//	POST /api/tenant-export/download {token} → application/gzip (attachment)
type Handler struct {
	Pool *pgxpool.Pool
}

type tokenBody struct {
	Token string `json:"token"`
}

func readToken(w http.ResponseWriter, r *http.Request) (string, bool) {
	var b tokenBody
	dec := json.NewDecoder(io.LimitReader(r.Body, 1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil || b.Token == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return "", false
	}
	return b.Token, true
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// Info validates a link (without using it up) for the download page.
func (h *Handler) Info(w http.ResponseWriter, r *http.Request) {
	tok, ok := readToken(w, r)
	if !ok {
		return
	}
	var g Grant
	err := pgx.BeginTxFunc(r.Context(), h.Pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		g, err = Inspect(r.Context(), tx, tok)
		return err
	})
	if errors.Is(err, ErrInvalidToken) {
		writeErr(w, http.StatusNotFound, "invalid_or_expired")
		return
	}
	if err != nil {
		log.Printf("tenantexport: info: %v", err)
		writeErr(w, http.StatusInternalServerError, "server_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"workspace_name": g.Name, "slug": g.Slug, "delete_after": g.DeleteAfter.UTC().Format(time.RFC3339),
	})
}

// Download validates (and, for a sign-in token, uses up) the token, records
// the download in the tenant's audit stream, and streams the archive.
func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	tok, ok := readToken(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var g Grant
	err := pgx.BeginTxFunc(ctx, h.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var err error
		if g, err = Redeem(ctx, tx, tok); err != nil {
			return err
		}
		return audit.RecordTx(ctx, tx, audit.Entry{
			TenantID: g.WorkspaceID, ActorUserID: g.UserID, ActorEmail: g.UserEmail,
			Action: "tenant_export.download", TargetType: "workspace", TargetID: g.WorkspaceID,
			Details: map[string]any{"via": g.Purpose},
		})
	})
	if errors.Is(err, ErrInvalidToken) {
		writeErr(w, http.StatusNotFound, "invalid_or_expired")
		return
	}
	if err != nil {
		log.Printf("tenantexport: download: %v", err)
		writeErr(w, http.StatusInternalServerError, "server_error")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="zecurity-`+g.Slug+`-logs-`+time.Now().UTC().Format("20060102")+`.tar.gz"`)
	err = pgx.BeginTxFunc(ctx, h.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		_, err := Write(ctx, tx, g.WorkspaceID, g.Slug, w)
		return err
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		// Headers are sent; the client sees a truncated archive (bad gzip).
		log.Printf("tenantexport: stream archive for workspace %s: %v", g.WorkspaceID, err)
	}
}
