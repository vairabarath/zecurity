package auditexport

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestAddressAllowed(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8":              true,
		"2606:4700:4700::1111": true,
		"127.0.0.1":            false,
		"10.1.2.3":             false,
		"172.16.0.1":           false,
		"192.168.1.1":          false,
		"169.254.169.254":      false, // cloud metadata
		"100.64.0.1":           false, // CGNAT
		"0.0.0.0":              false,
		"0.1.2.3":              false,
		"224.0.0.1":            false,
		"255.255.255.255":      false,
		"198.18.0.1":           false,
		"::1":                  false,
		"::":                   false,
		"fe80::1":              false,
		"fc00::1":              false,
		"fd12:3456::1":         false,
		"ff02::1":              false,
		"::ffff:127.0.0.1":     false, // IPv4-mapped loopback
		"::ffff:10.0.0.1":      false,
		"::ffff:8.8.8.8":       true,
		"64:ff9b::a00:1":       false, // NAT64 of 10.0.0.1
		"2002:a00:1::1":        false, // 6to4 of 10.0.0.1
		"2001:db8::1":          false,
	} {
		if got := AddressAllowed(netip.MustParseAddr(addr)); got != want {
			t.Errorf("AddressAllowed(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestValidateURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://siem.example.com/hook":            true,
		"https://siem.example.com:8088/services/x": true,
		"http://siem.example.com/hook":             false,
		"https://user:pw@siem.example.com/":        false,
		"https://127.0.0.1/hook":                   false,
		"https://[::1]/hook":                       false,
		"https://10.0.0.5/hook":                    false,
		"https://localhost/hook":                   false,
		"https:///nohost":                          false,
		"https://siem.example.com/#frag":           false,
		"ftp://siem.example.com/":                  false,
	} {
		if err := ValidateURL(raw, false); (err == nil) != ok {
			t.Errorf("ValidateURL(%q) err=%v, want ok=%v", raw, err, ok)
		}
	}
	if err := ValidateURL("https://127.0.0.1:9443/hook", true); err != nil {
		t.Errorf("dev override: %v", err)
	}
	if got := RedactURL("https://siem.example.com/services/T1/B2/secret?token=abc"); got != "https://siem.example.com" {
		t.Errorf("RedactURL = %q", got)
	}
}

// The signature is HMAC-SHA256(key, "<unix>." + body), as documented.
func TestSignature(t *testing.T) {
	key, body := []byte("zsk_test"), []byte(`{"a":1}`)
	at := time.Unix(1700000000, 0)
	m := hmac.New(sha256.New, key)
	m.Write([]byte("1700000000." + string(body)))
	want := "t=1700000000,v1=" + hex.EncodeToString(m.Sum(nil))
	if got := Signature(key, at, body); got != want {
		t.Fatalf("Signature = %s, want %s", got, want)
	}
}

// The dial-time guard refuses loopback even when the URL check is bypassed
// (a name that resolves to a private address, DNS rebinding), and no
// redirect is followed.
func TestClient_BlocksPrivateAtDialAndRedirects(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "https://example.com/", http.StatusFound)
	}))
	defer srv.Close()
	url := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	d := Destination{URL: url, SigningKey: []byte("k")}
	b := Batch{DeliveryID: "d", WorkspaceID: "w", Stream: "admin"}

	err := Webhook{}.Deliver(context.Background(), NewClient(ClientOptions{}), d, b)
	var de *DeliveryError
	if !errors.As(err, &de) || !strings.Contains(de.Msg, "not allowed") {
		t.Fatalf("guarded client: err = %v, want address not allowed", err)
	}
	if hits != 0 {
		t.Fatalf("request reached the loopback server")
	}
	if strings.Contains(err.Error(), srv.URL[len("http://"):]) {
		t.Fatalf("error leaks the destination: %v", err)
	}

	// Dev override: reaches it, and the 302 is a failure, not followed.
	err = Webhook{}.Deliver(context.Background(), NewClient(ClientOptions{AllowPrivate: true}), d, b)
	if !errors.As(err, &de) || de.Msg != "destination answered HTTP 302" {
		t.Fatalf("redirect: err = %v, want HTTP 302 failure", err)
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1 (redirect not followed)", hits)
	}
}

func TestValidateHeader(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		ok          bool
	}{
		{"Authorization", "Bearer x", true},
		{"X-Api-Key", "abc", true},
		{"Host", "evil", false},
		{"X-Zecurity-Signature", "x", false},
		{"Content-Length", "1", false},
		{"Bad Header", "x", false},
		{"Authorization", "a\r\nX: y", false},
		{"Authorization", "", false},
	} {
		if err := validateHeader(tc.name, tc.value); (err == nil) != tc.ok {
			t.Errorf("validateHeader(%q, %q) err=%v, want ok=%v", tc.name, tc.value, err, tc.ok)
		}
	}
}

func TestCapBytes(t *testing.T) {
	big := strings.Repeat("x", 600<<10)
	evs := []Event{{Action: big}, {Action: big}, {Action: "small"}}
	if got := capBytes(evs, MaxBatchBytes); len(got) != 1 {
		t.Fatalf("capBytes kept %d events, want 1", len(got))
	}
	huge := []Event{{Action: strings.Repeat("y", 2<<20)}}
	if got := capBytes(huge, MaxBatchBytes); len(got) != 1 {
		t.Fatalf("an oversized single event must still be sent")
	}
}
