package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.RWMutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock {
	return &fakeClock{now: t}
}

func (c *fakeClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func makeTestCert(t *testing.T, notBefore, notAfter time.Time) *tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "Test Controller",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	return &tls.Certificate{
		Certificate: [][]byte{derBytes},
		PrivateKey:  priv,
	}
}

// 1. TestNewControllerCertRotator
func TestNewControllerCertRotator(t *testing.T) {
	now := time.Now()
	cert := makeTestCert(t, now.Add(-time.Hour), now.Add(24*time.Hour))
	dummyGen := func(ctx context.Context) (*tls.Certificate, time.Time, time.Time, error) {
		return cert, now.Add(-time.Hour), now.Add(24 * time.Hour), nil
	}

	// Panics when cert is nil
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("expected panic when cert is nil, got none")
			}
		}()
		NewControllerCertRotator(dummyGen, nil, now.Add(-time.Hour), now.Add(24*time.Hour), nil)
	}()

	// Panics when notBefore is zero
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("expected panic when notBefore is zero, got none")
			}
		}()
		NewControllerCertRotator(dummyGen, cert, time.Time{}, now.Add(24*time.Hour), nil)
	}()

	// Panics when notAfter is zero
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("expected panic when notAfter is zero, got none")
			}
		}()
		NewControllerCertRotator(dummyGen, cert, now.Add(-time.Hour), time.Time{}, nil)
	}()

	// Defaults clock to non-nil when nil clock passed
	rotator := NewControllerCertRotator(dummyGen, cert, now.Add(-time.Hour), now.Add(24*time.Hour), nil)
	if rotator.clock == nil {
		t.Fatal("expected rotator.clock to be defaulted to non-nil")
	}
	gotCert, err := rotator.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate error: %v", err)
	}
	if gotCert != cert {
		t.Errorf("GetCertificate returned %v, want initial cert %v", gotCert, cert)
	}

	// GetCertificate returns errNoCertificateConfigured when cur is empty
	rotator.cur.Store(nil)
	gotNilCert, err := rotator.GetCertificate(nil)
	if err == nil {
		t.Errorf("expected error when cur is nil, got nil error and cert %v", gotNilCert)
	}
	if !errors.Is(err, errNoCertificateConfigured) {
		t.Errorf("got error %v, want errNoCertificateConfigured", err)
	}
	if gotNilCert != nil {
		t.Errorf("expected nil cert on error, got %v", gotNilCert)
	}
}

// 2. TestNeedsRotation
func TestNeedsRotation(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	notBefore := base
	notAfter := base.Add(30 * time.Minute)
	// Lifetime = 30m, 2/3 lifetime = 20m -> threshold = base + 20m
	threshold := base.Add(20 * time.Minute)

	cert := makeTestCert(t, notBefore, notAfter)
	rotator := NewControllerCertRotator(nil, cert, notBefore, notAfter, func() time.Time { return base })

	tests := []struct {
		name string
		now  time.Time
		want bool
	}{
		{
			name: "before threshold",
			now:  threshold.Add(-1 * time.Second),
			want: false,
		},
		{
			name: "exact threshold",
			now:  threshold,
			want: true,
		},
		{
			name: "after threshold",
			now:  threshold.Add(1 * time.Second),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rotator.needsRotation(tt.now)
			if got != tt.want {
				t.Errorf("needsRotation(%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}

	// current nil / cert nil -> true
	rotator.cur.Store(nil)
	if !rotator.needsRotation(base) {
		t.Errorf("needsRotation with nil current = false, want true")
	}
}

// TestNeedsRotationBackdatedNotBefore mirrors GenerateControllerServerTLS,
// which backdates NotBefore by 1h for clock skew. The 2/3 threshold must be
// measured from issuance, not from the backdated NotBefore; otherwise any
// TTL <= 30m is due for rotation the moment it is issued.
func TestNeedsRotationBackdatedNotBefore(t *testing.T) {
	const skew = time.Hour
	for _, ttl := range []time.Duration{10 * time.Minute, 30 * time.Minute, 2 * time.Hour, 7 * 24 * time.Hour} {
		t.Run(ttl.String(), func(t *testing.T) {
			issued := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			clk := newFakeClock(issued)
			wantDue := issued.Add(time.Duration(float64(ttl) * rotationRatio))

			// Initial certificate (constructor path).
			certA := makeTestCert(t, issued.Add(-skew), issued.Add(ttl))
			r := NewControllerCertRotator(nil, certA, issued.Add(-skew), issued.Add(ttl), clk.Now)
			if r.needsRotation(issued) {
				t.Fatalf("freshly issued cert is already due for rotation")
			}
			if got := r.nextDue(); !got.Equal(wantDue) {
				t.Fatalf("nextDue = %v, want %v (issuance + 2/3 ttl)", got, wantDue)
			}
			if r.needsRotation(wantDue.Add(-time.Second)) {
				t.Errorf("due 1s before issuance + 2/3 ttl")
			}
			if !r.needsRotation(wantDue) {
				t.Errorf("not due at issuance + 2/3 ttl")
			}

			// Rotated certificate (rotate path).
			clk.Advance(ttl)
			reissued := clk.Now()
			certB := makeTestCert(t, reissued.Add(-skew), reissued.Add(ttl))
			r.gen = func(context.Context) (*tls.Certificate, time.Time, time.Time, error) {
				return certB, reissued.Add(-skew), reissued.Add(ttl), nil
			}
			if err := r.rotate(context.Background()); err != nil {
				t.Fatalf("rotate: %v", err)
			}
			if r.needsRotation(reissued) {
				t.Fatalf("freshly rotated cert is already due for rotation")
			}
			if got, want := r.nextDue(), reissued.Add(time.Duration(float64(ttl)*rotationRatio)); !got.Equal(want) {
				t.Fatalf("nextDue after rotate = %v, want %v", got, want)
			}
		})
	}
}

// 3. TestRotateSwapAndFailure
func TestRotateSwapAndFailure(t *testing.T) {
	now := time.Now()
	clk := newFakeClock(now)
	certA := makeTestCert(t, now.Add(-time.Hour), now.Add(24*time.Hour))
	certB := makeTestCert(t, now, now.Add(24*time.Hour))

	ctx := context.Background()

	// Success case
	genSuccess := func(ctx context.Context) (*tls.Certificate, time.Time, time.Time, error) {
		return certB, now, now.Add(24 * time.Hour), nil
	}
	r := NewControllerCertRotator(genSuccess, certA, now.Add(-time.Hour), now.Add(24*time.Hour), clk.Now)

	if err := r.rotate(ctx); err != nil {
		t.Fatalf("rotate failed: %v", err)
	}
	gotCert, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if gotCert != certB {
		t.Errorf("GetCertificate after rotate = %v, want certB %v", gotCert, certB)
	}

	// Failure keep-old: gen returns error
	genError := func(ctx context.Context) (*tls.Certificate, time.Time, time.Time, error) {
		return nil, time.Time{}, time.Time{}, errors.New("boom")
	}
	rErr := NewControllerCertRotator(genError, certA, now.Add(-time.Hour), now.Add(24*time.Hour), clk.Now)
	if err := rErr.rotate(ctx); err == nil {
		t.Errorf("expected rotate error, got nil")
	}
	gotOld, _ := rErr.GetCertificate(nil)
	if gotOld != certA {
		t.Errorf("rotate on error changed cert to %v, want certA %v", gotOld, certA)
	}

	// Failure nil cert
	genNil := func(ctx context.Context) (*tls.Certificate, time.Time, time.Time, error) {
		return nil, now, now.Add(24 * time.Hour), nil
	}
	rNil := NewControllerCertRotator(genNil, certA, now.Add(-time.Hour), now.Add(24*time.Hour), clk.Now)
	if err := rNil.rotate(ctx); err == nil {
		t.Errorf("expected error for nil cert, got nil")
	}
	gotOld, _ = rNil.GetCertificate(nil)
	if gotOld != certA {
		t.Errorf("rotate on nil cert changed cert to %v, want certA %v", gotOld, certA)
	}

	// Failure zero notBefore
	genZeroNB := func(ctx context.Context) (*tls.Certificate, time.Time, time.Time, error) {
		return certB, time.Time{}, now.Add(24 * time.Hour), nil
	}
	rZeroNB := NewControllerCertRotator(genZeroNB, certA, now.Add(-time.Hour), now.Add(24*time.Hour), clk.Now)
	if err := rZeroNB.rotate(ctx); err == nil {
		t.Errorf("expected error for zero notBefore, got nil")
	}
	gotOld, _ = rZeroNB.GetCertificate(nil)
	if gotOld != certA {
		t.Errorf("rotate on zero notBefore changed cert to %v, want certA %v", gotOld, certA)
	}

	// Failure notAfter before now
	genExpired := func(ctx context.Context) (*tls.Certificate, time.Time, time.Time, error) {
		return certB, now.Add(-2 * time.Hour), now.Add(-time.Hour), nil
	}
	rExpired := NewControllerCertRotator(genExpired, certA, now.Add(-time.Hour), now.Add(24*time.Hour), clk.Now)
	if err := rExpired.rotate(ctx); err == nil {
		t.Errorf("expected error for expired notAfter, got nil")
	}
	gotOld, _ = rExpired.GetCertificate(nil)
	if gotOld != certA {
		t.Errorf("rotate on expired notAfter changed cert to %v, want certA %v", gotOld, certA)
	}
}

// 4. TestRunRotatesAtThreshold
func TestRunRotatesAtThreshold(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := newFakeClock(base)

	notBefore := base
	notAfter := base.Add(300 * time.Millisecond) // short lifetime for nextDue sleep in Run
	// 2/3 of 300ms = 200ms
	threshold := base.Add(200 * time.Millisecond)

	certA := makeTestCert(t, notBefore, notAfter)
	certB := makeTestCert(t, base.Add(200*time.Millisecond), base.Add(24*time.Hour))

	genCalled := make(chan struct{}, 1)
	gen := func(ctx context.Context) (*tls.Certificate, time.Time, time.Time, error) {
		select {
		case genCalled <- struct{}{}:
		default:
		}
		return certB, base.Add(200 * time.Millisecond), base.Add(24 * time.Hour), nil
	}

	rotator := NewControllerCertRotator(gen, certA, notBefore, notAfter, clk.Now)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		rotator.Run(ctx)
	}()

	// Advance clock past threshold
	clk.Advance(250 * time.Millisecond)
	_ = threshold

	// Poll until GetCertificate returns certB
	deadline := time.Now().Add(5 * time.Second)
	rotated := false
	for time.Now().Before(deadline) {
		got, err := rotator.GetCertificate(nil)
		if err == nil && got == certB {
			rotated = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !rotated {
		t.Fatalf("expected rotation to certB within deadline")
	}

	cancel()

	select {
	case <-runDone:
	case <-time.After(1 * time.Second):
		t.Fatalf("Run did not exit within 1s after context cancellation")
	}

	// CertB still served
	got, err := rotator.GetCertificate(nil)
	if err != nil || got != certB {
		t.Errorf("GetCertificate = %v (err %v), want certB %v", got, err, certB)
	}
}

// 5. TestRunKeepsCurrentOnGenFailure
func TestRunKeepsCurrentOnGenFailure(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := newFakeClock(base.Add(30 * time.Minute)) // already past threshold

	notBefore := base
	notAfter := base.Add(30 * time.Minute)

	certA := makeTestCert(t, notBefore, notAfter)
	genErr := func(ctx context.Context) (*tls.Certificate, time.Time, time.Time, error) {
		return nil, time.Time{}, time.Time{}, errors.New("generation failed")
	}

	rotator := NewControllerCertRotator(genErr, certA, notBefore, notAfter, clk.Now)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		rotator.Run(ctx)
	}()

	// Wait briefly (backoff sleep is 30s so Run is waiting on backoff timer or ctx.Done)
	time.Sleep(300 * time.Millisecond)

	got, err := rotator.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate error: %v", err)
	}
	if got != certA {
		t.Errorf("GetCertificate returned %v, want initial cert %v", got, certA)
	}

	cancel()

	select {
	case <-runDone:
	case <-time.After(1 * time.Second):
		t.Fatalf("Run did not exit promptly after context cancel")
	}

	gotAfter, _ := rotator.GetCertificate(nil)
	if gotAfter != certA {
		t.Errorf("GetCertificate after cancel = %v, want %v", gotAfter, certA)
	}
}

// 6. TestConcurrentGetCertificateDuringRotation (-race target)
func TestConcurrentGetCertificateDuringRotation(t *testing.T) {
	now := time.Now()
	certA := makeTestCert(t, now.Add(-time.Hour), now.Add(24*time.Hour))

	gen := func(ctx context.Context) (*tls.Certificate, time.Time, time.Time, error) {
		n := time.Now()
		c := makeTestCert(t, n.Add(-time.Hour), n.Add(24*time.Hour))
		return c, n.Add(-time.Hour), n.Add(24 * time.Hour), nil
	}

	rotator := NewControllerCertRotator(gen, certA, now.Add(-time.Hour), now.Add(24*time.Hour), time.Now)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	// 8 reader goroutines
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
					c, err := rotator.GetCertificate(nil)
					if err != nil {
						t.Errorf("concurrent GetCertificate returned error: %v", err)
					}
					if c == nil {
						t.Errorf("concurrent GetCertificate returned nil cert")
					}
				}
			}
		}()
	}

	// Driver performing 200 swaps
	for i := 0; i < 200; i++ {
		if err := rotator.rotate(ctx); err != nil {
			t.Fatalf("rotate iteration %d failed: %v", i, err)
		}
	}

	cancel()
	wg.Wait()

	if rotator.current() == nil || rotator.current().cert == nil {
		t.Errorf("expected non-nil current certificate at end of test")
	}
}

// TestRotatorHandshakeRotation tests that a tls.Server configured with
// ControllerCertRotator.GetCertificate continues accepting handshakes across
// certificate rotation and even after the original certificate has expired.
func TestRotatorHandshakeRotation(t *testing.T) {
	// Build a test CA
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	caSerial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		t.Fatalf("generate CA serial: %v", err)
	}

	caTemplate := &x509.Certificate{
		SerialNumber: caSerial,
		Subject: pkix.Name{
			CommonName: "Test Controller Root CA",
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}

	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)

	spiffeURI, err := url.Parse("spiffe://zecurity.in/controller/global")
	if err != nil {
		t.Fatalf("parse SPIFFE URI: %v", err)
	}

	// Helper to mint a leaf certificate signed by the CA with a given TTL
	mintLeaf := func(ttl time.Duration) (*tls.Certificate, time.Time, time.Time, error) {
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, time.Time{}, time.Time{}, err
		}

		leafSerial, err := rand.Int(rand.Reader, serialLimit)
		if err != nil {
			return nil, time.Time{}, time.Time{}, err
		}

		now := time.Now()
		notBefore := now.Add(-10 * time.Millisecond)
		notAfter := now.Add(ttl)

		template := &x509.Certificate{
			SerialNumber: leafSerial,
			Subject: pkix.Name{
				CommonName: "controller.test",
			},
			DNSNames:              []string{"controller.test"},
			URIs:                  []*url.URL{spiffeURI},
			NotBefore:             notBefore,
			NotAfter:              notAfter,
			KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			BasicConstraintsValid: true,
			IsCA:                  false,
		}

		leafDER, err := x509.CreateCertificate(rand.Reader, template, caCert, &leafKey.PublicKey, caKey)
		if err != nil {
			return nil, time.Time{}, time.Time{}, err
		}

		return &tls.Certificate{
			Certificate: [][]byte{leafDER},
			PrivateKey:  leafKey,
		}, notBefore, notAfter, nil
	}

	const ttl = 4 * time.Second
	initialCert, initialNotBefore, initialNotAfter, err := mintLeaf(ttl)
	if err != nil {
		t.Fatalf("mint initial leaf: %v", err)
	}

	initialParsed, err := x509.ParseCertificate(initialCert.Certificate[0])
	if err != nil {
		t.Fatalf("parse initial cert: %v", err)
	}
	initialSerial := initialParsed.SerialNumber.String()

	gen := func(ctx context.Context) (*tls.Certificate, time.Time, time.Time, error) {
		return mintLeaf(ttl)
	}

	rotator := NewControllerCertRotator(gen, initialCert, initialNotBefore, initialNotAfter, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start TLS server
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer tcpListener.Close()

	tlsConfig := &tls.Config{
		GetCertificate: rotator.GetCertificate,
		MinVersion:     tls.VersionTLS13,
	}
	tlsListener := tls.NewListener(tcpListener, tlsConfig)
	defer tlsListener.Close()

	go func() {
		for {
			conn, err := tlsListener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				tlsConn, ok := c.(*tls.Conn)
				if ok {
					_ = tlsConn.HandshakeContext(ctx)
				}
			}(conn)
		}
	}()

	// Client handshake helper
	dialAndGetSerial := func() (string, error) {
		dialer := &net.Dialer{Timeout: 2 * time.Second}
		tlsConn, err := tls.DialWithDialer(dialer, "tcp", tlsListener.Addr().String(), &tls.Config{
			RootCAs:    caPool,
			ServerName: "controller.test",
			MinVersion: tls.VersionTLS13,
		})
		if err != nil {
			return "", err
		}
		defer tlsConn.Close()

		state := tlsConn.ConnectionState()
		if len(state.PeerCertificates) == 0 {
			return "", errors.New("no peer certificates returned")
		}
		return state.PeerCertificates[0].SerialNumber.String(), nil
	}

	// 5a. Immediately: handshake succeeds and serial == initialSerial
	firstSerial, err := dialAndGetSerial()
	if err != nil {
		t.Fatalf("initial handshake failed: %v", err)
	}
	if firstSerial != initialSerial {
		t.Fatalf("initial handshake serial = %s, want %s", firstSerial, initialSerial)
	}

	// Start Run in a goroutine now that initial handshake has been verified
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		rotator.Run(ctx)
	}()

	// 5b. Poll until rotator.GetCertificate returns a cert with a new serial
	pollDeadline := time.Now().Add(6 * time.Second)
	var rotatedSerial string
	for time.Now().Before(pollDeadline) {
		currentCert, err := rotator.GetCertificate(nil)
		if err == nil && currentCert != nil && len(currentCert.Certificate) > 0 {
			parsed, pErr := x509.ParseCertificate(currentCert.Certificate[0])
			if pErr == nil && parsed.SerialNumber.String() != initialSerial {
				rotatedSerial = parsed.SerialNumber.String()
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rotatedSerial == "" {
		t.Fatalf("certificate was not rotated before deadline")
	}

	// 5c. Handshake again: succeeds and serial == rotatedSerial
	secondSerial, err := dialAndGetSerial()
	if err != nil {
		t.Fatalf("handshake after rotation failed: %v", err)
	}
	if secondSerial == initialSerial {
		t.Fatalf("handshake after rotation returned initial serial %s", initialSerial)
	}
	if secondSerial != rotatedSerial {
		t.Fatalf("handshake after rotation returned serial %s, want %s", secondSerial, rotatedSerial)
	}

	// 5d. Wait until time.Now() is past the INITIAL cert's NotAfter (wait bounded ~3s)
	for !time.Now().After(initialNotAfter) {
		time.Sleep(50 * time.Millisecond)
	}

	// Handshake past original NotAfter: must still succeed with valid rotated cert
	thirdSerial, err := dialAndGetSerial()
	if err != nil {
		t.Fatalf("handshake past original NotAfter failed: %v", err)
	}
	if thirdSerial == initialSerial {
		t.Fatalf("handshake past original NotAfter returned expired initial serial %s", initialSerial)
	}

	cancel()
	_ = tlsListener.Close()
	_ = tcpListener.Close()

	select {
	case <-runDone:
	case <-time.After(1 * time.Second):
		t.Fatalf("rotator.Run did not stop within 1s after cancel")
	}
}
