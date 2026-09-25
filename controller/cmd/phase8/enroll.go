package main

// enroll.go -- the device half of the Phase 8 harness.
//
// Enrolling a device normally runs through `zecurity-client login`, which opens
// a browser for Google consent. The Phase 8 test device is a headless LAN box,
// and Phase 8 proves posture -> Device Profile -> Resource Policy -> ACL ->
// Connector, not the OAuth handshake. So this mints the access token the OAuth
// callback would have produced and then performs the SAME EnrollDevice RPC the
// client performs: same P-384 key, same CSR proving possession, same server-side
// path (cert signing, SPIFFE assignment, policy notification).
//
// The substitution is the browser consent step, and nothing else. The output is
// exactly the payload the client's own `PostLoginState` IPC message carries, so
// the daemon that consumes it cannot tell the difference.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"crypto/tls"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
)

// cmdEnroll enrolls a device and prints the state the client daemon persists.
func cmdEnroll() {
	if len(os.Args) < 6 {
		fmt.Fprintln(os.Stderr,
			"usage: phase8 enroll <access-token> <controller-grpc> <controller-http> <device-name>")
		os.Exit(2)
	}
	accessToken := os.Args[2]
	grpcAddr := os.Args[3]
	httpBase := os.Args[4]
	deviceName := os.Args[5]

	caPEM, err := fetchCA(httpBase)
	if err != nil {
		log.Fatalf("fetch controller CA: %v", err)
	}

	// P-384, matching the client's rcgen PKCS_ECDSA_P384_SHA384 key.
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		log.Fatalf("generate device key: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		log.Fatalf("marshal device key: %v", err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))

	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: deviceName},
		SignatureAlgorithm: x509.ECDSAWithSHA384,
	}, key)
	if err != nil {
		log.Fatalf("create CSR: %v", err)
	}
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		log.Fatal("controller CA PEM was not parseable")
	}
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(
		credentials.NewTLS(&tls.Config{RootCAs: pool, ServerName: hostOf(grpcAddr)}),
	))
	if err != nil {
		log.Fatalf("dial controller: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := clientv1.NewClientServiceClient(conn).EnrollDevice(ctx, &clientv1.EnrollDeviceRequest{
		AccessToken: accessToken,
		CsrPem:      csrPEM,
		DeviceName:  deviceName,
		Os:          "linux",
	})
	if err != nil {
		log.Fatalf("enroll device: %v", err)
	}

	notAfter, err := certNotAfter(resp.GetCertificatePem())
	if err != nil {
		log.Fatalf("parse issued certificate: %v", err)
	}

	emit(map[string]any{
		"device_id":       resp.GetDeviceId(),
		"spiffe_id":       resp.GetSpiffeId(),
		"certificate_pem": resp.GetCertificatePem(),
		"private_key_pem": keyPEM,
		"ca_cert_pem":     resp.GetWorkspaceCaPem() + "\n" + resp.GetIntermediateCaPem(),
		"cert_expires_at": notAfter,
		"hostname":        deviceName,
		"os":              "linux",
	})
}

func fetchCA(httpBase string) (string, error) {
	resp, err := http.Get(httpBase + "/ca.crt")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func certNotAfter(certPEM string) (int64, error) {
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return 0, fmt.Errorf("certificate was not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return 0, err
	}
	return cert.NotAfter.Unix(), nil
}

func hostOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}
