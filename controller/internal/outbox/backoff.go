package outbox

import (
	"fmt"
	"math"
	"time"
)

const MaxBackoff = 5 * time.Minute

// RetryPolicy controls exponential retry scheduling.
type RetryPolicy struct {
	Base       time.Duration
	Max        time.Duration
	Jitter     float64
	MaxRetries int
}

// DefaultRetryPolicy returns the initial Phase 4 defaults.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		Base:       time.Second,
		Max:        MaxBackoff,
		Jitter:     0.20,
		MaxRetries: DefaultMaxRetries,
	}
}

func (p RetryPolicy) Validate() error {
	if p.Base <= 0 {
		return fmt.Errorf("outbox retry base must be positive")
	}

	if p.Max <= 0 {
		return fmt.Errorf("outbox retry max must be positive")
	}

	if p.Max > MaxBackoff {
		return fmt.Errorf(
			"outbox retry max must not exceed %s",
			MaxBackoff,
		)
	}

	if p.Jitter < 0 || p.Jitter > 1 {
		return fmt.Errorf(
			"outbox retry jitter must be between 0 and 1: %f",
			p.Jitter,
		)
	}

	if p.MaxRetries < 1 || p.MaxRetries > 1000 {
		return fmt.Errorf(
			"outbox max retries must be between 1 and 1000: %d",
			p.MaxRetries,
		)
	}

	return nil
}

// Backoff calculates the exponential retry delay before jitter.
//
// delay = min(max, 2^retryCount * base)
func (p RetryPolicy) Backoff(retryCount int) time.Duration {
	if retryCount < 0 {
		retryCount = 0
	}

	// Avoid overflowing time.Duration when calculating 2^retryCount.
	exponent := math.Min(float64(retryCount), 62)
	multiplier := math.Pow(2, exponent)

	delay := time.Duration(float64(p.Base) * multiplier)

	if delay <= 0 || delay > p.Max {
		return p.Max
	}

	return delay
}

type JitterSource interface {
	Apply(duration time.Duration, fraction float64) time.Duration
}

type NoJitter struct{}

func (NoJitter) Apply(duration time.Duration, _ float64) time.Duration {
	return duration
}

type FixedJitter struct {
	Factor float64
}

func (j FixedJitter) Apply(
	duration time.Duration,
	fraction float64,
) time.Duration {
	return time.Duration(
		float64(duration) * (1 + j.Factor*fraction),
	)
}
