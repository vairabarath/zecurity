package resolvers

// Helpers for the SCIM bearer-token resolvers (Sprint 17 / ADR-025).
// Kept out of idp.resolvers.go so gqlgen's codegen (which relocates non-resolver
// code) never drops them.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/scim"
)

// scimTokenToGQL converts the internal token representation to the GraphQL
// representation.
//
// Notice that Token does not contain plaintext. Plaintext only exists in
// MintResult during mint/rotate and is deliberately never part of ScimToken.
func scimTokenToGQL(t scim.Token) *graph.ScimToken {
	return &graph.ScimToken{
		ID:           t.ID,
		WorkspaceID:  t.WorkspaceID,
		ConnectionID: t.ConnectionID,
		Label:        t.Label,
		CreatedBy:    t.CreatedBy,
		CreatedAt:    t.CreatedAt,
		LastUsedAt:   t.LastUsedAt,
		ExpiresAt:    t.ExpiresAt,
		RevokedAt:    t.RevokedAt,
	}
}

// validateSCIMConnection makes the workspace/connection relationship explicit
// at the resolver boundary.
//
// The token store also enforces this relationship. Keeping the check here
// gives GraphQL callers a clean workspace-scoped error before mint/rotate/list.
func (r *Resolver) validateSCIMConnection(
	ctx context.Context,
	workspaceID string,
	connectionID string,
) error {
	conn, err := r.IdpStore.GetByID(ctx, connectionID)
	if err != nil {
		if errors.Is(err, errors.New("identity connection not found")) {
			return fmt.Errorf("SCIM connection not found")
		}
		return fmt.Errorf("get SCIM connection: %w", err)
	}

	if conn.TenantID == nil || *conn.TenantID != workspaceID {
		return fmt.Errorf("SCIM connection does not belong to this workspace")
	}

	return nil
}

func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// conflictToGQL converts an internal ConflictRecord to the GraphQL ScimConflict
// representation, mapping the optional resolution fields and timestamps.
func conflictToGQL(c *scim.ConflictRecord) *graph.ScimConflict {
	return &graph.ScimConflict{
		ID:               c.ID,
		WorkspaceID:      c.WorkspaceID,
		ConnectionID:     c.ConnectionID,
		UserID:           c.UserID,
		CanonicalKey:     c.CanonicalKey,
		ScimExternalID:   stringPtrOrNil(c.ScimExternalID),
		Status:           c.Status,
		ResolutionReason: stringPtrOrNil(c.ResolutionReason),
		CreatedAt:        parseConflictTime(c.CreatedAt),
		ResolvedAt:       parseConflictOptTime(c.ResolvedAt),
	}
}

func parseConflictTime(s string) time.Time {
	t, err := time.Parse("2006-01-02T15:04:05Z", s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func parseConflictOptTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse("2006-01-02T15:04:05Z", s)
	if err != nil {
		return nil
	}
	return &t
}
