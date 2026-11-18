package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// Relay link capacity (Sprint 23 B2, F5; Decision Record D-42, refined
// 2026-11-18). An operator sets a relay's usable link throughput, in bytes
// per second, in the link's tighter direction; the read API derives
// headroom = capacity − (rate to clients + rate to connectors). No relay,
// proto or connector change.
//
//	PUT /provider/relays/{id}/link-capacity  relay-ops, super-admin
//	    {"bps": <bytes per second> | null, "reason": "..."}  → 204
//
// null clears it ("Not configured"). The change commits with its audit row
// (before/after, F17). Setting the current value again is a no-op (not
// audited). Revoked and deleted relays are refused (terminal).

// MaxLinkCapacityBps bounds the value: 400 Gbit/s in bytes per second,
// beyond any single relay link; it catches unit mistakes (bits vs bytes,
// an extra digit) before they reach the console.
const MaxLinkCapacityBps int64 = 50_000_000_000

var errInvalidCapacity = errors.New("invalid link capacity")

// SetLinkCapacity sets (or with nil clears) a relay's link capacity under
// the relay's row lock. changed=false (no audit) when the value is
// unchanged. audit gets the value before and after.
func (s *Store) SetLinkCapacity(ctx context.Context, relayID string, bps *int64,
	audit func(ctx context.Context, tx pgx.Tx, before, after *int64) error) (changed bool, err error) {
	if bps != nil && (*bps <= 0 || *bps > MaxLinkCapacityBps) {
		return false, errInvalidCapacity
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var status string
	var before *int64
	err = tx.QueryRow(ctx, `SELECT status, link_capacity_bps FROM relays WHERE id = $1 FOR UPDATE`, relayID).Scan(&status, &before)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrRelayNotFound
	}
	if err != nil {
		return false, fmt.Errorf("lock relay: %w", err)
	}
	switch status {
	case "deleted":
		return false, errAlreadyDeleted
	case "revoked":
		return false, errAlreadyRevoked
	}
	if sameCapacity(before, bps) {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE relays SET link_capacity_bps = $2, updated_at = NOW() WHERE id = $1`, relayID, bps); err != nil {
		return false, fmt.Errorf("set link capacity: %w", err)
	}
	if audit != nil {
		if err := audit(ctx, tx, before, bps); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit link capacity: %w", err)
	}
	return true, nil
}

func sameCapacity(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

type linkCapacityRequest struct {
	// BPS is kept raw so a missing field (invalid) differs from null (clear).
	BPS    json.RawMessage `json:"bps"`
	Reason string          `json:"reason"`
}

// SetLinkCapacity: PUT /provider/relays/{id}/link-capacity.
func (h *AdminHandler) SetLinkCapacity(w http.ResponseWriter, r *http.Request) {
	actor, relayID, ok := h.authorizeRelayAction(w, r, provider.ActionRelayCapacitySet, h.Authz.CanSetRelayCapacity)
	if !ok {
		return
	}
	var req linkCapacityRequest
	if !provider.DecodeJSON(w, r, &req) || len(req.BPS) == 0 {
		provider.WriteError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var bps *int64
	if !bytes.Equal(bytes.TrimSpace(req.BPS), []byte("null")) {
		var v int64
		if err := json.Unmarshal(req.BPS, &v); err != nil {
			provider.WriteError(w, http.StatusBadRequest, "invalid_capacity")
			return
		}
		bps = &v
	}
	reason, ok := requireReason(w, req.Reason)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := h.Store.SetLinkCapacity(ctx, relayID, bps, func(ctx context.Context, tx pgx.Tx, before, after *int64) error {
		return h.ProviderStore.InsertAuditTx(ctx, tx, provider.AuditEntry{
			ProviderUserID: &actor.UserID,
			ProviderEmail:  actor.Email,
			Action:         provider.ActionRelayCapacitySet,
			TargetType:     "relay",
			TargetID:       relayID,
			Details: provider.WithChange(map[string]any{"reason": reason},
				map[string]any{"link_capacity_bps": before},
				map[string]any{"link_capacity_bps": after}),
			IPAddress: provider.ClientIP(r),
		})
	})
	if errors.Is(err, errInvalidCapacity) {
		provider.WriteError(w, http.StatusBadRequest, "invalid_capacity")
		return
	}
	if err != nil {
		writePoolError(w, "set link capacity", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
