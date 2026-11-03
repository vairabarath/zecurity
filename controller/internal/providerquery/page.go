package providerquery

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// Pagination: keyset on (created_at, id), newest first. The cursor is opaque
// to clients: base64url of {"t": created_at, "id": id} for the last row of
// the previous page.

const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// ClampLimit maps a requested page size into [1, MaxLimit]; zero or negative
// means DefaultLimit. Handlers reject non-numeric or negative input with 400
// before calling; this is the server-side clamp.
func ClampLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultLimit
	case n > MaxLimit:
		return MaxLimit
	default:
		return n
	}
}

// Page is one page of a list. NextCursor is nil on the last page.
type Page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// Keyset is a decoded cursor position.
type Keyset struct {
	CreatedAt time.Time
	ID        string
}

type cursorWire struct {
	T  string `json:"t"`
	ID string `json:"id"`
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsUUID reports whether s is a canonical UUID string.
func IsUUID(s string) bool { return uuidRE.MatchString(s) }

// EncodeCursor returns the opaque cursor for a row.
func EncodeCursor(createdAt time.Time, id string) string {
	b, _ := json.Marshal(cursorWire{T: createdAt.UTC().Format(time.RFC3339Nano), ID: id})
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor parses a cursor; "" means the first page (nil, nil).
func DecodeCursor(s string) (*Keyset, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: encoding", ErrInvalidCursor)
	}
	var w cursorWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("%w: payload", ErrInvalidCursor)
	}
	t, err := time.Parse(time.RFC3339Nano, w.T)
	if err != nil || !IsUUID(w.ID) {
		return nil, fmt.Errorf("%w: fields", ErrInvalidCursor)
	}
	return &Keyset{CreatedAt: t, ID: w.ID}, nil
}

// keysetArgs turns an optional cursor into SQL args for
// "($n::timestamptz IS NULL OR (created_at, id) < ($n, $n+1::uuid))".
func keysetArgs(k *Keyset) (any, any) {
	if k == nil {
		return nil, nil
	}
	return k.CreatedAt, k.ID
}

// utcPtr normalizes an optional timestamp to UTC (RFC 3339 UTC in JSON).
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
