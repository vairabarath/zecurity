package provider

import "net/http"

// Exported HTTP helpers for other packages serving /provider/* routes (the
// relay admin handler, Sprint 22 Phase A). They are thin wrappers over the
// helpers the provider auth and operator handlers already use, so every
// provider route answers errors, client IPs and request bodies the same way.

// WriteJSON writes body as JSON with Cache-Control: no-store.
func WriteJSON(w http.ResponseWriter, status int, body any) { writeAuthJSON(w, status, body) }

// WriteError writes {"error": code}.
func WriteError(w http.ResponseWriter, status int, code string) { writeAuthError(w, status, code) }

// WriteErrorDetail writes {"error": code, "detail": detail}. detail must be
// safe to show the caller (validation messages only, never internal errors).
func WriteErrorDetail(w http.ResponseWriter, status int, code, detail string) {
	writeAuthJSON(w, status, map[string]string{"error": code, "detail": detail})
}

// ClientIP is the connection's remote host. X-Forwarded-For and similar
// headers are NOT trusted: there is no trusted-proxy boundary configured.
func ClientIP(r *http.Request) string { return clientIP(r) }

// DecodeJSON decodes a bounded (4 KiB) JSON body into dst, rejecting unknown
// fields. False means the body was missing, too large or malformed.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeAuthBody(w, r, dst)
}
