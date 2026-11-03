package relay

import (
	"context"
	"log"
	"net/http"

	"github.com/yourorg/ztna/controller/internal/pki"
)

// relayCRLGenerator is the narrow slice of the PKI service the relay CRL endpoint
// needs (interface segregation + easy testing).
type relayCRLGenerator interface {
	GenerateRelayCRL(ctx context.Context, revoked []pki.RevokedEntry) ([]byte, error)
}

// RelayCRLHandler serves the platform relay CRL (Intermediate-CA-signed, DER) at
// GET /relay.crl. On each request it fetches the revoked, still-unexpired relay
// serials from the store and asks the PKI service to sign a fresh CRL — same
// on-demand model as /ca.crl. Unauthenticated: a CRL is self-authenticating signed
// data (consumers verify the Intermediate-CA signature).
func RelayCRLHandler(gen relayCRLGenerator, store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		revoked, err := store.ListRevokedRelaySerials(r.Context())
		if err != nil {
			log.Printf("relay crl endpoint: list revoked serials: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		entries := make([]pki.RevokedEntry, 0, len(revoked))
		for _, rs := range revoked {
			entries = append(entries, pki.RevokedEntry{Serial: rs.Serial, RevokedAt: rs.RevokedAt})
		}

		der, err := gen.GenerateRelayCRL(r.Context(), entries)
		if err != nil {
			log.Printf("relay crl endpoint: generate CRL: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/pkix-crl")
		w.Header().Set("Content-Disposition", `attachment; filename="relay.crl"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(der)
	}
}
