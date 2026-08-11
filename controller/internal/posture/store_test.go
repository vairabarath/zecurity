package posture

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCreateProfileRejectsBlankNameBeforeDatabaseAccess(t *testing.T) {
	store := NewStore(nil)
	for _, name := range []string{"", "   ", "\t\n"} {
		if _, err := store.CreateProfile(context.Background(), uuid.New(), name, true); !errors.Is(err, ErrInvalidProfileName) {
			t.Errorf("CreateProfile(%q) error = %v, want %v", name, err, ErrInvalidProfileName)
		}
	}
}

func TestUpdateProfileManualTrustRejectsDisableBeforeDatabaseAccess(t *testing.T) {
	store := NewStore(nil)
	if _, err := store.UpdateProfileManualTrust(context.Background(), uuid.New(), uuid.New(), false); !errors.Is(err, ErrNoVerificationMethod) {
		t.Errorf("UpdateProfileManualTrust(enabled=false) error = %v, want %v", err, ErrNoVerificationMethod)
	}
}
