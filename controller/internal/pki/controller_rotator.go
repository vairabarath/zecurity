package pki

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"sync/atomic"
	"time"
)

// errNoCertificateConfigured is returned by GetCertificate if no certificate is loaded.
var errNoCertificateConfigured = errors.New("no controller certificate configured")

// rotatedCert wraps an in-memory TLS certificate with its validity boundaries.
type rotatedCert struct {
	cert      *tls.Certificate
	notBefore time.Time
	notAfter  time.Time
	// issuedAt is the rotator clock when the certificate was stored. The
	// rotation threshold is measured from here rather than notBefore because
	// GenerateControllerServerTLS backdates notBefore by 1h for clock skew;
	// measuring from the backdated value would make any TTL <= 30m due for
	// rotation the moment it is issued.
	issuedAt time.Time
}

// rotationDue returns issuance + rotationRatio*(notAfter - issuance), where
// issuance is the later of notBefore and issuedAt.
func (c *rotatedCert) rotationDue() time.Time {
	start := c.notBefore
	if c.issuedAt.After(start) {
		start = c.issuedAt
	}
	lifetime := c.notAfter.Sub(start)
	return start.Add(time.Duration(float64(lifetime) * rotationRatio))
}

// ControllerCertRotator manages periodic in-memory rotation of the controller's
// gRPC server certificate without restarting the server or breaking active TLS sessions.
type ControllerCertRotator struct {
	gen   func(ctx context.Context) (*tls.Certificate, time.Time, time.Time, error) // cert, notBefore, notAfter
	cur   atomic.Pointer[rotatedCert]
	clock func() time.Time
}

// NewControllerCertRotator creates a new ControllerCertRotator initialized with
// the initial certificate and its validity boundaries. It panics if cert is nil,
// or if notBefore or notAfter is zero, guaranteeing that the rotator never serves
// an invalid certificate after startup.
func NewControllerCertRotator(
	gen func(ctx context.Context) (*tls.Certificate, time.Time, time.Time, error), // cert, notBefore, notAfter
	cert *tls.Certificate, // initial certificate (already parsed)
	notBefore time.Time,
	notAfter time.Time,
	clock func() time.Time,
) *ControllerCertRotator {
	if cert == nil || notBefore.IsZero() || notAfter.IsZero() {
		panic("initial certificate and validity times must not be nil or zero")
	}
	if clock == nil {
		clock = time.Now
	}

	r := &ControllerCertRotator{
		gen:   gen,
		clock: clock,
	}
	r.cur.Store(&rotatedCert{
		cert:      cert,
		notBefore: notBefore,
		notAfter:  notAfter,
		issuedAt:  clock(),
	})
	return r
}

// GetCertificate returns the current active server certificate for TLS handshakes.
// It satisfies the tls.Config.GetCertificate signature. It never returns (nil, nil);
// if no certificate is loaded, it returns an error to fail the handshake cleanly.
func (r *ControllerCertRotator) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	current := r.cur.Load()
	if current == nil || current.cert == nil {
		return nil, errNoCertificateConfigured
	}
	return current.cert, nil
}

// current returns the current rotatedCert container.
func (r *ControllerCertRotator) current() *rotatedCert {
	return r.cur.Load()
}

// rotationRatio defines the fraction of certificate lifetime elapsed before
// rotation is triggered (2/3 of the certificate's lifetime).
const rotationRatio = float64(2) / 3

// needsRotation returns true if the current certificate is nil or if now has
// reached 2/3 of its lifetime measured from issuance (see rotationDue).
func (r *ControllerCertRotator) needsRotation(now time.Time) bool {
	current := r.current()
	if current == nil || current.cert == nil {
		return true
	}
	return !now.Before(current.rotationDue())
}

// rotate regenerates the certificate via gen and atomically stores the result.
// It validates that the returned certificate is non-nil and currently valid before swapping.
func (r *ControllerCertRotator) rotate(ctx context.Context) error {
	cert, notBefore, notAfter, err := r.gen(ctx)
	if err != nil {
		return err
	}

	if cert == nil {
		return errors.New("generated certificate is nil")
	}
	if notBefore.IsZero() {
		return errors.New("generated certificate notBefore is zero")
	}
	now := r.clock()
	if notAfter.Before(now) {
		return errors.New("generated certificate notAfter is before current time")
	}

	r.cur.Store(&rotatedCert{
		cert:      cert,
		notBefore: notBefore,
		notAfter:  notAfter,
		issuedAt:  now,
	})
	return nil
}

const (
	// backoffMin is the initial backoff delay on rotation failure.
	backoffMin = 30 * time.Second
	// backoffMax is the maximum backoff delay on repeated rotation failures.
	backoffMax = 10 * time.Minute
	// checkInterval is the maximum idle sleep duration between rotation checks.
	checkInterval = time.Minute
)

// nextDue returns the threshold time when the current certificate reaches 2/3 of its lifetime.
// If no certificate is currently loaded, it returns the zero time.
func (r *ControllerCertRotator) nextDue() time.Time {
	current := r.current()
	if current == nil || current.cert == nil {
		return time.Time{}
	}
	return current.rotationDue()
}

// Run periodically checks if the certificate is due for rotation and rotates it.
// On generation failure, it logs the error, keeps serving the current certificate,
// and retries with capped exponential backoff.
func (r *ControllerCertRotator) Run(ctx context.Context) {
	backoff := backoffMin

	for {
		now := r.clock()
		if r.needsRotation(now) {
			if err := r.rotate(ctx); err != nil {
				log.Printf("controller cert rotation failed: %v", err)

				timer := time.NewTimer(backoff)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}

				backoff *= 2
				if backoff > backoffMax {
					backoff = backoffMax
				}
				continue
			}

			// Reset backoff on successful rotation.
			backoff = backoffMin
		}

		// Calculate sleep duration until next due time, capped at checkInterval.
		sleepDuration := checkInterval
		due := r.nextDue()
		if !due.IsZero() {
			untilDue := due.Sub(r.clock())
			if untilDue > 0 && untilDue < sleepDuration {
				sleepDuration = untilDue
			}
		}

		timer := time.NewTimer(sleepDuration)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
