package connector

import (
	"context"
	"log"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CAEndpointHandler serves the intermediate CA certificate as PEM on GET /ca.crt.
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
