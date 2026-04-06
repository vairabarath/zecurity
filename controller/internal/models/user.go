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
}
