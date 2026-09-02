package scim

import (
	"encoding/json"
	"fmt"
	"io"
)

// jsonDecode reads a JSON value from r into v.
func jsonDecode(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}

// errf formats an error.
func errf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
