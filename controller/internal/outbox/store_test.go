package outbox

import (
	"errors"
	"strings"
	"testing"
)

func TestTruncateError(t *testing.T) {
	tests := []struct {
		name      string
		message   string
		wantBytes int
	}{
		{
			name:      "nil error",
			message:   "",
			wantBytes: 0,
		},
		{
			name:      "short error",
			message:   "something went wrong",
			wantBytes: len("something went wrong"),
		},
		{
			name:      "exactly 4096 bytes",
			message:   strings.Repeat("x", 4096),
			wantBytes: 4096,
		},
		{
			name:      "larger than 4096 bytes",
			message:   strings.Repeat("x", 5000),
			wantBytes: 4096,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			if tt.name != "nil error" {
				err = errors.New(tt.message)
			}

			got := truncateError(err)

			if tt.name == "nil error" {
				if got != nil {
					t.Fatalf("truncateError(nil) = %q, want nil", *got)
				}
				return
			}

			if got == nil {
				t.Fatal("truncateError() returned nil")
			}

			if gotBytes := len([]byte(*got)); gotBytes != tt.wantBytes {
				t.Fatalf(
					"truncateError() returned %d bytes, want %d",
					gotBytes,
					tt.wantBytes,
				)
			}

			if tt.wantBytes == len([]byte(tt.message)) {
				if *got != tt.message {
					t.Fatal("error message was unexpectedly modified")
				}
				return
			}

			if *got != tt.message[:4096] {
				t.Fatal("error was not truncated to the first 4096 bytes")
			}
		})
	}
}
