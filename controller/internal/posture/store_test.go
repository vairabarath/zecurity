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
		if _, err := store.CreateProfile(context.Background(), uuid.New(), name); !errors.Is(err, ErrInvalidProfileName) {
			t.Errorf("CreateProfile(%q) error = %v, want %v", name, err, ErrInvalidProfileName)
		}
	}
}
