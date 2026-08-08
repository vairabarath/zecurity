package resolvers

// Addressing helpers for the resource mutations (PENDING-14 / Sprint 16).
//
// These live outside resource.resolvers.go on purpose: gqlgen owns every
// *.resolvers.go file and strips any non-resolver function out of them on the
// next `make gqlgen`, leaving it commented at the bottom of the file.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// blankToNil normalises an optional string input: absent and present-but-empty
// both mean "not supplied". Without this, `host: ""` would insert an empty
// string, which is NOT NULL and so satisfies resources_addressable_check while
// being useless to dial.
func blankToNil(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	return &t
}

// validateAddressing enforces exactly one addressing mode. The DB constraint
// only requires at least one; allowing both would leave the connector with an
// IP and a name that can disagree, and nothing to say which wins.
func validateAddressing(host, hostname *string) error {
	switch {
	case host == nil && hostname == nil:
		return fmt.Errorf("one of host or hostname is required")
	case host != nil && hostname != nil:
		return fmt.Errorf("host and hostname are mutually exclusive")
	}
	return nil
}

// validateResolverJSON rejects a resolver the connector could never act on,
// returning a readable GraphQL error instead of a raw constraint violation.
// Requires a JSON object with a non-empty string "type"; the config shape is
// intentionally not checked here (Phase 6 owns it) and unknown keys are
// tolerated.
func validateResolverJSON(raw *string) error {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*raw), &obj); err != nil {
		return fmt.Errorf("resolver must be a JSON object: %w", err)
	}
	rawType, ok := obj["type"]
	if !ok {
		return fmt.Errorf(`resolver must contain a "type" key`)
	}
	var t string
	if err := json.Unmarshal(rawType, &t); err != nil || strings.TrimSpace(t) == "" {
		return fmt.Errorf(`resolver "type" must be a non-empty string`)
	}
	return nil
}
