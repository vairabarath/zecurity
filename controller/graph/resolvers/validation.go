package resolvers

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yourorg/ztna/controller/internal/apperr"
)

// maxAgentNameLen bounds connector/shield names. The DB column is TEXT (no
// limit), so the cap is enforced here. Keep this in sync with the maxLength on
// the admin InstallCommandModal input.
const maxAgentNameLen = 64

// validateAgentName trims and validates a user-supplied connector/shield name.
// The name is stored as a per-tenant unique identifier and shown in the console,
// CLI, and logs, so we bound its length and reject control characters. It is NOT
// an XSS vector (React escapes it on render and it never reaches a shell), so we
// intentionally allow ordinary punctuation/Unicode rather than an allowlist that
// would reject legitimate names. kind ("connector"/"shield") is used only for
// the error message. Returns the trimmed name to store.
func validateAgentName(kind, raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", apperr.UserErrorf("%s name is required", kind)
	}
	if utf8.RuneCountInString(name) > maxAgentNameLen {
		return "", apperr.UserErrorf("%s name must be %d characters or fewer", kind, maxAgentNameLen)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", apperr.UserErrorf("%s name contains invalid control characters", kind)
		}
	}
	return name, nil
}
