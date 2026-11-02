package resolvers

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yourorg/ztna/controller/internal/apperr"
)

// errDuplicateGroupName replaces the raw unique-index error (which named the
// constraint) with a message an admin can act on.
var errDuplicateGroupName = apperr.UserErrorf("a group with this name already exists")

// validGroupName trims a group name and refuses a blank one (a group named
// "   " was accepted before).
func validGroupName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", apperr.UserErrorf("group name is required")
	}
	return name, nil
}

// isDuplicateGroupName reports a violation of the per-workspace unique name
// index on manual groups (migration 034, idx_groups_manual_name).
func isDuplicateGroupName(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "idx_groups_manual_name"
}
