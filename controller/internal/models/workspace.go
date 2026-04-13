package models

import "time"

type Workspace struct {
	ID         string    `db:"id"`
	Slug       string    `db:"slug"`
	Name       string    `db:"name"`
	Status     string    `db:"status"`
	CACertPEM  *string   `db:"ca_cert_pem"`
	CreatedAt  time.Time `db:"created_at"`
	UpdatedAt  time.Time `db:"updated_at"`
}
