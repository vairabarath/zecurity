package resolvers

import (
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
