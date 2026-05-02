package resolvers

import (
	"fmt"
	"time"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/invitation"
)

func invitationToGQL(inv *invitation.Invitation) *graph.Invitation {
	return &graph.Invitation{
		ID:        inv.ID,
		Email:     inv.Email,
		Status:    inv.Status,
		ExpiresAt: inv.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		CreatedAt: inv.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

func scanClientDevice(row scanner) (*graph.ClientDevice, error) {
	var (
		id, userID, name, os string
		spiffeID             *string
		certNotAfter         *time.Time
		lastSeenAt           *time.Time
		createdAt            time.Time
		revokedAt            *time.Time
	)
	if err := row.Scan(&id, &userID, &name, &os, &spiffeID, &certNotAfter, &lastSeenAt, &createdAt, &revokedAt); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	d := &graph.ClientDevice{
		ID:         id,
		UserID:     userID,
		Name:       name,
		CommonName: name,
		Os:         os,
		SpiffeID:   spiffeID,
		CreatedAt:  createdAt.UTC().Format(time.RFC3339),
	}
	if certNotAfter != nil {
		s := certNotAfter.UTC().Format(time.RFC3339)
		d.CertNotAfter = &s
	}
	if lastSeenAt != nil {
		s := lastSeenAt.UTC().Format(time.RFC3339)
		d.LastSeenAt = &s
	}
	if revokedAt != nil {
		s := revokedAt.UTC().Format(time.RFC3339)
		d.RevokedAt = &s
	}
	return d, nil
}
