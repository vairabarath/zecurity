package scim

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// userSchema is the SCIM 2.0 User resource URN.
const userSchema = "urn:ietf:params:scim:schemas:core:2.0:User"

// errorSchema is the SCIM error envelope URN (RFC 7644 §3.12).
const errorSchema = "urn:ietf:params:scim:api:messages:error"

// Meta carries SCIM resource metadata. Version is the canonical resource
// version — we surface users.identity_generation as meta.version so a future
// If-Match optimistic-concurrency check can be enabled without a breaking
// change (ADR-025 §8).
type Meta struct {
	ResourceType string `json:"resourceType"`
	Created      string `json:"created,omitempty"`
	LastModified string `json:"lastModified,omitempty"`
	Version      string `json:"version,omitempty"`
	Location     string `json:"location,omitempty"`
}

// User is the minimal SCIM 2.0 User resource we emit. Directory-owned
// attributes only (name/email/active per the Phase 5 scope); Zecurity-owned
// attributes (role, group membership) are never serialized here.
type User struct {
	Schemas  []string `json:"schemas"`
	ID       string   `json:"id"`
	UserName string   `json:"userName,omitempty"`
	ExternalID string  `json:"externalId,omitempty"`
	Active   bool     `json:"active"`
	Emails   []Email  `json:"emails,omitempty"`
	Meta     Meta     `json:"meta"`
}

// Email is a SCIM email attribute.
type Email struct {
	Primary bool   `json:"primary,omitempty"`
	Value   string `json:"value,omitempty"`
	Type    string `json:"type,omitempty"`
}

// ListResponse is the SCIM 2.0 query/list envelope (RFC 7644 §3.4.2).
type ListResponse struct {
	Schemas      []string `json:"schemas"`
	TotalResults int      `json:"totalResults"`
	StartIndex   int      `json:"startIndex"`
	ItemsPerPage int      `json:"itemsPerPage"`
	Resources    []User   `json:"Resources"`
}

// SCIMError is a typed SCIM protocol error. The handler renders it as an RFC
// 7644 error envelope with the given HTTP status; ScimType carries the
// protocol-level scimType (e.g. "identity_conflict") when relevant.
type SCIMError struct {
	Status   int
	ScimType string
	Detail   string
}

func (e *SCIMError) Error() string { return e.Detail }

// newSCIMError builds a SCIMError.
func newSCIMError(status int, scimType, detail string) *SCIMError {
	return &SCIMError{Status: status, ScimType: scimType, Detail: detail}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/scim+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeSCIMError renders a SCIMError as an RFC 7644 error envelope.
func writeSCIMError(w http.ResponseWriter, e *SCIMError) {
	if e == nil {
		e = newSCIMError(http.StatusInternalServerError, "", "unknown error")
	}
	status := e.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	env := map[string]any{
		"schemas": []string{errorSchema},
		"detail":  e.Detail,
		"status":  strconv.Itoa(status),
	}
	if e.ScimType != "" {
		env["scimType"] = e.ScimType
	}
	writeJSON(w, status, env)
}

// writeUser renders a single SCIM User resource (200).
func writeUser(w http.ResponseWriter, u User) {
	writeJSON(w, http.StatusOK, u)
}

// writeUserCreated renders a provisioned SCIM User resource (201).
func writeUserCreated(w http.ResponseWriter, u User) {
	writeJSON(w, http.StatusCreated, u)
}

// writeList renders a SCIM list response (200).
func writeList(w http.ResponseWriter, users []User) {
	writeJSON(w, http.StatusOK, ListResponse{
		Schemas:      []string{"urn:ietf:params:scim:api:messages:listResponse"},
		TotalResults: len(users),
		StartIndex:   1,
		ItemsPerPage: len(users),
		Resources:    users,
	})
}
