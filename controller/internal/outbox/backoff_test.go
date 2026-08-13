package outbox

import (
	"fmt"
	"testing"
	"time"
)

func TestRetryPolicyBackoff(t *testing.T) {
	policy := RetryPolicy{
		Base:       time.Second,
		Max:        5 * time.Minute,
		Jitter:     0,
		MaxRetries: 100,
	}

	tests := []struct {
		retryCount int
		want       time.Duration
	}{
		{retryCount: 0, want: 1 * time.Second},
		{retryCount: 1, want: 2 * time.Second},
		{retryCount: 2, want: 4 * time.Second},
		{retryCount: 3, want: 8 * time.Second},
		{retryCount: 4, want: 16 * time.Second},
		{retryCount: 5, want: 32 * time.Second},
		{retryCount: 6, want: 64 * time.Second},
		{retryCount: 7, want: 128 * time.Second},
		{retryCount: 8, want: 256 * time.Second},
		{retryCount: 9, want: 5 * time.Minute},
		{retryCount: 10, want: 5 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(
			fmt.Sprintf("retry_%d", tt.retryCount),
			func(t *testing.T) {
				got := policy.Backoff(tt.retryCount)

				if got != tt.want {
					t.Fatalf(
						"Backoff(%d) = %s, want %s",
						tt.retryCount,
						got,
						tt.want,
					)
				}
			},
		)
	}
}

func TestRetryPolicyBackoffNegativeRetryCount(t *testing.T) {
	policy := RetryPolicy{
		Base:       time.Second,
		Max:        5 * time.Minute,
		Jitter:     0,
		MaxRetries: 100,
	}

	got := policy.Backoff(-1)

	if got != time.Second {
		t.Fatalf("Backoff(-1) = %s, want 1s", got)
	}
}

func TestRetryPolicyValidate(t *testing.T) {
	valid := RetryPolicy{
		Base:       time.Second,
		Max:        5 * time.Minute,
		Jitter:     0.20,
		MaxRetries: 100,
	}

	if err := valid.Validate(); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}

	tests := []struct {
		name   string
		policy RetryPolicy
	}{
		{
			name: "zero base",
			policy: RetryPolicy{
				Base:       0,
				Max:        5 * time.Minute,
				Jitter:     0.20,
				MaxRetries: 100,
			},
		},
		{
			name: "zero max",
			policy: RetryPolicy{
				Base:       time.Second,
				Max:        0,
				Jitter:     0.20,
				MaxRetries: 100,
			},
		},
		{
			name: "max exceeds contract",
			policy: RetryPolicy{
				Base:       time.Second,
				Max:        6 * time.Minute,
				Jitter:     0.20,
				MaxRetries: 100,
			},
		},
		{
			name: "negative jitter",
			policy: RetryPolicy{
				Base:       time.Second,
				Max:        5 * time.Minute,
				Jitter:     -0.1,
				MaxRetries: 100,
			},
		},
		{
			name: "jitter greater than one",
			policy: RetryPolicy{
				Base:       time.Second,
				Max:        5 * time.Minute,
				Jitter:     1.1,
				MaxRetries: 100,
			},
		},
		{
			name: "zero retries",
			policy: RetryPolicy{
				Base:       time.Second,
				Max:        5 * time.Minute,
				Jitter:     0.20,
				MaxRetries: 0,
			},
		},
		{
			name: "too many retries",
			policy: RetryPolicy{
				Base:       time.Second,
				Max:        5 * time.Minute,
				Jitter:     0.20,
				MaxRetries: 1001,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.policy.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestFixedJitter(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		fraction float64
		factor   float64
		want     time.Duration
	}{
		{
			name:     "no jitter",
			duration: time.Second,
			fraction: 0.20,
			factor:   0,
			want:     time.Second,
		},
		{
			name:     "positive jitter",
			duration: time.Second,
			fraction: 0.20,
			factor:   1,
			want:     1200 * time.Millisecond,
		},
		{
			name:     "negative jitter",
			duration: time.Second,
			fraction: 0.20,
			factor:   -1,
			want:     800 * time.Millisecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jitter := FixedJitter{Factor: tt.factor}

			got := jitter.Apply(tt.duration, tt.fraction)

			if got != tt.want {
				t.Fatalf(
					"Apply(%s, %f) = %s, want %s",
					tt.duration,
					tt.fraction,
					got,
					tt.want,
				)
			}
		})
	}
}
