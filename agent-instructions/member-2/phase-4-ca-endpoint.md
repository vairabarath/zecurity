# Phase 4 — CA Certificate HTTP Endpoint

## Objective

Create an HTTP handler that serves the Intermediate CA certificate at `GET /ca.crt`. This is a public, unauthenticated endpoint used by the Rust connector during enrollment to establish initial trust (the connector verifies the SHA-256 fingerprint against the JWT claim before trusting this cert).

---

## Prerequisites

- **Phase 2** completed (Config struct, for package existence)
- PKI service interface stable (`controller/internal/pki/service.go`)

---

## File to Create

```
controller/internal/connector/ca_endpoint.go
```

---

## Implementation

**File: `controller/internal/connector/ca_endpoint.go`**

```go
package connector

import (
	"context"
	"log"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CAEndpointHandler returns an HTTP handler that serves the Intermediate CA
// certificate as PEM on GET /ca.crt.
//
// This is a public, unauthenticated endpoint. The Rust connector fetches it
// during enrollment to establish initial TLS trust. The connector then verifies
// the SHA-256 fingerprint of the DER-encoded cert against the ca_fingerprint
// claim in the enrollment JWT — a mismatch aborts enrollment (possible MITM).
//
// The cert is read from the ca_intermediate table on each request.
// No caching — the intermediate CA changes rarely, and this endpoint
// is only hit once per connector enrollment.
//
// Called by: main.go (Phase 5) — registered as mux.HandleFunc("/ca.crt", ...)
func CAEndpointHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var certPEM string
		err := pool.QueryRow(
			context.Background(),
			`SELECT certificate_pem FROM ca_intermediate LIMIT 1`,
		).Scan(&certPEM)
		if err != nil {
			log.Printf("ca endpoint: failed to read intermediate CA: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("Content-Disposition", `attachment; filename="ca.crt"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(certPEM))
	}
}
```

---

## Design Decisions

1. **Reads from DB directly** rather than through the PKI service interface. The PKI `Service` interface doesn't expose a "get intermediate cert" method, and adding one just for this would mean modifying Member 3's domain. A simple `SELECT` on `ca_intermediate` is cleaner.

2. **No caching.** This endpoint is hit once per connector enrollment — not a hot path. The DB read is a single-row fetch from a table that has exactly one row.

3. **`*pgxpool.Pool` parameter** instead of `pki.Service`. Keeps the dependency minimal and avoids coupling to the PKI interface for a simple read.

4. **Content-Type `application/x-pem-file`** is the standard MIME type for PEM certificates. The `Content-Disposition` header suggests the filename for clients that save the response.

5. **No authentication.** The CA certificate is public information — it's the trust anchor, not a secret. The secret is the private key (never exposed). This is the same as any public CA distributing its root/intermediate cert.

---

## Verification

After Phase 5 wires this into main.go:

```bash
# Start the controller
cd controller && go run ./cmd/server/

# In another terminal
curl -v http://localhost:8080/ca.crt
```

Expected:
- HTTP 200
- `Content-Type: application/x-pem-file`
- Body is a PEM certificate starting with `-----BEGIN CERTIFICATE-----`

For now (before Phase 5):

```bash
cd controller && go build ./internal/connector/...
```

- [ ] File exists at `controller/internal/connector/ca_endpoint.go`
- [ ] Package is `package connector`
- [ ] Returns `http.HandlerFunc`
- [ ] Only handles `GET` — rejects other methods
- [ ] Reads from `ca_intermediate` table
- [ ] Sets correct Content-Type header
- [ ] `go build ./internal/connector/...` passes

---

## DO NOT TOUCH

- `controller/internal/pki/service.go` — Do not modify the Service interface. This handler doesn't use it.
- `controller/internal/pki/intermediate.go` — Sprint 1 intermediate CA setup. Do not modify.
- `controller/cmd/server/main.go` — Route registration happens in Phase 5.

---

## After This Phase

Proceed to Phase 5 (main.go wiring).
