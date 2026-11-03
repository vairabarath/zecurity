package identity

import (
	"errors"
	"testing"
)

func TestCheckGeneration(t *testing.T) {
	cases := []struct {
		name       string
		tokenGen   int
		currentGen int
		wantErr    bool
	}{
		{"equal generations pass", 3, 3, false},
		{"token behind is revoked", 2, 3, true},
		{"token far behind is revoked", 1, 9, true},
		{"legacy token (0) is accepted", 0, 5, false},
		{"token ahead is accepted", 4, 3, false}, // never happens, but must not falsely revoke
	}
	for _, c := range cases {
		err := CheckGeneration(c.tokenGen, c.currentGen)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: expected error, got nil", c.name)
				continue
			}
			if !errors.Is(err, ErrSessionRevoked) {
				t.Errorf("%s: expected ErrSessionRevoked, got %v", c.name, err)
			}
		} else if err != nil {
			t.Errorf("%s: expected nil, got %v", c.name, err)
		}
	}
}
