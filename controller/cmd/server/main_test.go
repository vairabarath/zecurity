package main

import (
	// "os"
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
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

func TestHTTPServerShutdown(t *testing.T) {
	handler := http.NewServeMux()
	handler.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	server := &http.Server{
		Handler: handler,
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()

		if err := server.Serve(listener); err != nil &&
			err != http.ErrServerClosed {
			t.Errorf("serve: %v", err)
		}
	}()

	shutdownCtx, cancel := context.WithTimeout(
		context.Background(),
		5*time.Second,
	)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Success.

	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
}
