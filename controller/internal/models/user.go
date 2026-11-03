package models

import "time"

type User struct {
	ID          string     `db:"id"`
	TenantID    string     `db:"tenant_id"`
	Email       string     `db:"email"`
	Provider    string     `db:"provider"`
	ProviderSub string     `db:"provider_sub"`
	Role        string     `db:"role"`
	Status      string     `db:"status"`
	LastLoginAt *time.Time `db:"last_login_at"`
	CreatedAt   time.Time  `db:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at"`
	// Provisioning provenance (ADR-025 §4). Both NOT NULL with defaults
	// (migration 034): ProvisionedBy is the immutable creation origin,
	// ProvisioningOwner the mutable current authority. Every query that feeds
	// these to GraphQL MUST select them — an empty string would read to the UI
	// as "not directory-managed", which is a wrong answer, not a missing one.
	ProvisionedBy     string `db:"provisioned_by"`
	ProvisioningOwner string `db:"provisioning_owner"`
}
