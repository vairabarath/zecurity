package providerquery

import (
	"encoding/base64"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestClampLimit(t *testing.T) {
	for in, want := range map[int]int{-5: 50, 0: 50, 1: 1, 50: 50, 200: 200, 201: 200, 100000: 200} {
		if got := ClampLimit(in); got != want {
			t.Errorf("ClampLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 5, 9, 30, 15, 123456000, time.FixedZone("IST", 19800))
	id := "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	k, err := DecodeCursor(EncodeCursor(at, id))
	if err != nil {
		t.Fatal(err)
	}
	if !k.CreatedAt.Equal(at) || k.ID != id {
		t.Fatalf("round trip = %+v", k)
	}
	if k, err := DecodeCursor(""); k != nil || err != nil {
		t.Fatalf(`DecodeCursor("") = %v, %v`, k, err)
	}
}

func TestDecodeCursor_RejectsMalformed(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, c := range map[string]string{
		"not base64":   "%%%",
		"not json":     enc("nope"),
		"bad time":     enc(`{"t":"yesterday","id":"0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"}`),
		"bad id":       enc(`{"t":"2026-10-05T09:30:15Z","id":"1; DROP TABLE relays"}`),
		"missing id":   enc(`{"t":"2026-10-05T09:30:15Z"}`),
		"empty object": enc(`{}`),
	} {
		if _, err := DecodeCursor(c); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("%s: err = %v, want ErrInvalidCursor", name, err)
		}
	}
}

func TestIsUUID(t *testing.T) {
	if !IsUUID("0F1E2D3C-4B5A-6978-8796-A5B4C3D2E1F0") || IsUUID("x") || IsUUID("0f1e2d3c4b5a69788796a5b4c3d2e1f0") {
		t.Fatal("IsUUID wrong")
	}
}

// D-04: query services stay independent of HTTP. Checks the full transitive
// dependency set of the package (test files excluded).
func TestPackageDoesNotImportNetHTTP(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		switch {
		case dep == "net/http",
			strings.HasSuffix(dep, "/internal/relay"),
			strings.HasSuffix(dep, "/internal/provider"):
			t.Errorf("providerquery depends on %s", dep)
		}
	}
}
