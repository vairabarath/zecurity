package identity

import (
	"errors"
	"testing"
)

func TestCheckLifecycle(t *testing.T) {
	cases := []struct {
		status  string
		wantErr bool
	}{
		{"active", false},
		{"suspended", true},
		{"locked", true},
		{"deleted", true},
		{"", true},        // unknown state → fail closed
		{"invited", true}, // invite lifecycle lives in workspace_members, not here
	}
	for _, c := range cases {
		err := CheckLifecycle(c.status)
		if c.wantErr {
			if err == nil {
				t.Errorf("status %q: expected error, got nil", c.status)
				continue
			}
			if !errors.Is(err, ErrUserNotActive) {
				t.Errorf("status %q: expected ErrUserNotActive, got %v", c.status, err)
			}
		} else if err != nil {
			t.Errorf("status %q: expected nil, got %v", c.status, err)
		}
	}
}
