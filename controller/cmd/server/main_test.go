package main

import (
	// "os"
	"testing"
)

func TestEnvOrInt(t *testing.T) {

	tests := []struct {
		name     string
		value    *string
		fallback int
		want     int
	}{
		{
			name:     "unset uses fallback",
			value:    nil,
			fallback: 30,
			want:     30,
		},
		{
			name:     "valid integer",
			value:    stringPtr("42"),
			fallback: 30,
			want:     42,
		},
		{
			name:     "invalid integer",
			value:    stringPtr("abc"),
			fallback: 30,
			want:     30,
		},
		{
			name:     "zero uses fallback",
			value:    stringPtr("0"),
			fallback: 30,
			want:     30,
		},
		{
			name:     "negative uses fallback",
			value:    stringPtr("-5"),
			fallback: 30,
			want:     30,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const key = "TEST_ENV_OR_INT"
			if tt.value == nil {
				t.Setenv(key, "")
			} else {
				t.Setenv(key, *tt.value)
			}

			got := envOrInt(key, tt.fallback)
			if got != tt.want {
				t.Fatalf("envOrInt(%q) = %d, want %d", key, got, tt.want)
			}
		})
	}
}

func stringPtr(s string) *string {
	return &s
}
