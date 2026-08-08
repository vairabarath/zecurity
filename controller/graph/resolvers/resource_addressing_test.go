package resolvers

import "testing"

func sp(s string) *string { return &s }

// TestBlankToNil covers the normalisation that stops a present-but-empty string
// from being stored. Without it, `host: ""` inserts an empty string, which is
// NOT NULL and therefore satisfies resources_addressable_check while being
// impossible to dial — the resource would look addressable and never work.
func TestBlankToNil(t *testing.T) {
	cases := []struct {
		name string
		in   *string
		want *string
	}{
		{"nil stays nil", nil, nil},
		{"empty becomes nil", sp(""), nil},
		{"whitespace becomes nil", sp("   \t "), nil},
		{"value passes through", sp("10.0.0.5"), sp("10.0.0.5")},
		{"value is trimmed", sp("  db.internal  "), sp("db.internal")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := blankToNil(tc.in)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("want nil, got %q", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("want %q, got nil", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("want %q, got %q", *tc.want, *got)
			}
		})
	}
}

// TestValidateAddressing pins the "exactly one" rule. The DB constraint only
// requires at least one; the API is stricter on purpose, because a resource
// carrying both an IP and a name gives the connector two targets that can
// disagree and no rule for which wins.
func TestValidateAddressing(t *testing.T) {
	cases := []struct {
		name     string
		host     *string
		hostname *string
		wantErr  bool
	}{
		{"host only", sp("10.0.0.5"), nil, false},
		{"hostname only", nil, sp("db.internal"), false},
		{"neither is rejected", nil, nil, true},
		{"both is rejected", sp("10.0.0.5"), sp("db.internal"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAddressing(tc.host, tc.hostname)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateAddressing(%v, %v) err = %v, wantErr = %v",
					tc.host, tc.hostname, err, tc.wantErr)
			}
		})
	}
}

// TestValidateResolverJSON checks the API-level gate that turns a bad resolver
// into a readable GraphQL error instead of a raw Postgres constraint violation.
// It deliberately does NOT validate the config shape — Phase 6 owns that — so
// unknown keys and arbitrary config values must be accepted.
func TestValidateResolverJSON(t *testing.T) {
	cases := []struct {
		name    string
		in      *string
		wantErr bool
	}{
		{"nil is fine", nil, false},
		{"empty is fine", sp(""), false},
		{"whitespace is fine", sp("   "), false},
		{"minimal valid", sp(`{"type":"dns"}`), false},
		{"valid with config", sp(`{"type":"dns","config":{"server":"10.0.0.53"}}`), false},
		{"unknown keys tolerated", sp(`{"type":"static","address":"1.2.3.4","future":1}`), false},

		{"not json", sp(`nonsense`), true},
		{"json array", sp(`["dns"]`), true},
		{"json string", sp(`"dns"`), true},
		{"json number", sp(`42`), true},
		{"object without type", sp(`{"config":{}}`), true},
		{"type is empty", sp(`{"type":""}`), true},
		{"type is whitespace", sp(`{"type":"  "}`), true},
		{"type is not a string", sp(`{"type":123}`), true},
		{"type is null", sp(`{"type":null}`), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateResolverJSON(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateResolverJSON(%v) err = %v, wantErr = %v", tc.in, err, tc.wantErr)
			}
		})
	}
}
