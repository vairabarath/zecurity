package identity

import (
	"errors"
	"fmt"
)

// ErrUserNotActive is returned when a resolved user's canonical lifecycle state
// forbids login. Callers fail closed and surface a generic auth failure — the
// specific state (locked vs suspended vs deleted) is never leaked to the client.
var ErrUserNotActive = errors.New("user is not active")

// CheckLifecycle gates login on the canonical users.status (ADR-023): lifecycle
// lives on the user, independent of any external identity or IdP. Only 'active'
// proceeds; 'suspended', 'locked', and 'deleted' are rejected. Invite lifecycle
// is separate — it lives in workspace_members.status and is handled at
// provisioning time, not here.
func CheckLifecycle(status string) error {
	if status == "active" {
		return nil
	}
	return fmt.Errorf("%w (status=%q)", ErrUserNotActive, status)
}
