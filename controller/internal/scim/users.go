package scim

import (
	"context"
	"net/http"
	"regexp"
	"strings"
)

// Router returns the SCIM 2.0 HTTP handler subtree mounted at /scim/v2. All
// /Users (and the discovery endpoints) routes sit behind the SCIM bearer-auth
// middleware, which binds (workspace_id, connection_id) onto the request context
// via TokenFromContext. The DirectoryService never derives scope from the payload.
func (s *Store) Router(ds *DirectoryService) http.Handler {
	mux := http.NewServeMux()

	users := &userHandler{store: s, ds: ds}
	// Go 1.22+ method+path patterns.
	mux.HandleFunc("POST /scim/v2/Users", users.handlePost)
	mux.HandleFunc("GET /scim/v2/Users", users.handleList)
	mux.HandleFunc("GET /scim/v2/Users/{id}", users.handleGet)
	mux.HandleFunc("PUT /scim/v2/Users/{id}", users.handlePut)
	mux.HandleFunc("PATCH /scim/v2/Users/{id}", users.handlePatch)
	// Deprovision (DELETE / active=false) is Phase 6 — explicitly rejected so the
	// boundary is visible to IdPs rather than silently accepted.
	mux.HandleFunc("DELETE /scim/v2/Users/{id}", users.handleDeleteNotImplemented)

	// Read-only discovery endpoints so Okta/Entra do not error on connection test.
	mux.HandleFunc("GET /scim/v2/ServiceProviderConfig", handleServiceProviderConfig)
	mux.HandleFunc("GET /scim/v2/ResourceTypes", handleResourceTypes)
	mux.HandleFunc("GET /scim/v2/Schemas", handleSchemas)
	mux.HandleFunc("GET /scim/v2/.well-known/scim-configuration", handleServiceProviderConfig)

	return s.AuthMiddleware(mux)
}

// userHandler dispatches SCIM User operations, extracting scope from the
// authenticated token and delegating to DirectoryService.
type userHandler struct {
	store *Store
	ds    *DirectoryService
}

func (h *userHandler) scope(ctx context.Context) (*scope, *SCIMError) {
	tok := TokenFromContext(ctx)
	if tok == nil {
		return nil, newSCIMError(401, "", "missing SCIM token")
	}
	return h.ds.resolveScope(ctx, tok.WorkspaceID, tok.ConnectionID)
}

// syncInstanceFor resolves the current sync instance id for a connection, if any.
// A fresh /Users POST in Phase 5 carries no sync instance, so we return "" and the
// provisioner writes a NULL sync_instance_id (reconciled on reconnect in Phase 9).
func (h *userHandler) syncInstanceFor(ctx context.Context, sc *scope) string {
	var id string
	_ = h.store.pool.QueryRow(ctx,
		`SELECT id::text FROM scim_sync_instances
		  WHERE workspace_id = $1 AND connection_id = $2
		  ORDER BY created_at DESC LIMIT 1`,
		sc.workspaceID, sc.connectionID,
	).Scan(&id)
	return id
}

func (h *userHandler) handlePost(w http.ResponseWriter, r *http.Request) {
	sc, serr := h.scope(r.Context())
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	var resource map[string]any
	if err := decodeJSON(r, &resource); err != nil {
		writeSCIMError(w, newSCIMError(400, "invalidValue", "invalid SCIM User resource: "+err.Error()))
		return
	}
	res, serr := h.ds.Provision(r.Context(), sc, resource, h.syncInstanceFor(r.Context(), sc))
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	if res.conflict {
		// Already returned as 409 by Provision; guard for callers.
		writeSCIMError(w, newSCIMError(409, "identity_conflict", "identity conflict; admin approval required"))
		return
	}
	if res.created {
		writeUserCreated(w, res.user)
	} else {
		writeUser(w, res.user)
	}
}

func (h *userHandler) handleList(w http.ResponseWriter, r *http.Request) {
	sc, serr := h.scope(r.Context())
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	filter := r.URL.Query().Get("filter")
	if filter == "" {
		// No filter → empty collection (IdPs always filter before listing).
		writeList(w, nil)
		return
	}
	users, serr := h.ds.Filter(r.Context(), sc, filter)
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	writeList(w, users)
}

func (h *userHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	sc, serr := h.scope(r.Context())
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	id := r.PathValue("id")
	u, serr := h.ds.Get(r.Context(), sc, id)
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	writeUser(w, *u)
}

func (h *userHandler) handlePut(w http.ResponseWriter, r *http.Request) {
	sc, serr := h.scope(r.Context())
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	id := r.PathValue("id")
	var resource map[string]any
	if err := decodeJSON(r, &resource); err != nil {
		writeSCIMError(w, newSCIMError(400, "invalidValue", "invalid SCIM User resource: "+err.Error()))
		return
	}
	patch, perr := patchFromResource(resource)
	if perr != nil {
		writeSCIMError(w, newSCIMError(400, "invalidValue", perr.Error()))
		return
	}
	u, serr := h.ds.Update(r.Context(), sc, id, patch, h.syncInstanceFor(r.Context(), sc))
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	writeUser(w, *u)
}

func (h *userHandler) handlePatch(w http.ResponseWriter, r *http.Request) {
	sc, serr := h.scope(r.Context())
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	id := r.PathValue("id")
	var body struct {
		Schemas []string       `json:"schemas"`
		Ops     []map[string]any `json:"Operations"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeSCIMError(w, newSCIMError(400, "invalidValue", "invalid SCIM PATCH: "+err.Error()))
		return
	}
	patch, perr := patchFromOps(body.Ops)
	if perr != nil {
		writeSCIMError(w, newSCIMError(400, "invalidValue", perr.Error()))
		return
	}
	u, serr := h.ds.Update(r.Context(), sc, id, patch, h.syncInstanceFor(r.Context(), sc))
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	writeUser(w, *u)
}

func (h *userHandler) handleDeleteNotImplemented(w http.ResponseWriter, r *http.Request) {
	// Phase 6 takes over deprovision. For Phase 5 we fail clearly so a misconfigured
	// IdP is visible rather than silently no-op'd.
	writeSCIMError(w, newSCIMError(501, "", "SCIM deprovision is not implemented in this release (Phase 6)"))
}

// ── request parsing ───────────────────────────────────────────────────────────

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return jsonDecode(r.Body, v)
}

// userPatch is the normalized set of directory-owned mutations Phase 5 may apply.
// Unsupported attributes are rejected earlier (supportedDirectoryAttr).
type userPatch struct {
	Email string
	Active *bool
	// Attributes carries the raw attribute names touched, for the unowned-
	// attribute rejection check.
	Attributes []attrChange
}

type attrChange struct {
	Name  string
	Value any
}

// patchFromResource derives a userPatch from a full PUT resource (email + active).
func patchFromResource(resource map[string]any) (*userPatch, error) {
	p := &userPatch{}
	if v, ok := resource["userName"].(string); ok {
		p.Email = strings.TrimSpace(v)
		p.Attributes = append(p.Attributes, attrChange{Name: "userName", Value: v})
	}
	if v, ok := resource["active"].(bool); ok {
		b := v
		p.Active = &b
		p.Attributes = append(p.Attributes, attrChange{Name: "active", Value: v})
	}
	return p, nil
}

// patchFromOps derives a userPatch from RFC 7644 PATCH Operations.
func patchFromOps(ops []map[string]any) (*userPatch, error) {
	p := &userPatch{}
	for _, op := range ops {
		opType, _ := op["op"].(string)
		opType = strings.ToUpper(opType)
		path, _ := op["path"].(string)
		value := op["value"]
		switch opType {
		case "REPLACE", "ADD":
			applyPatchValue(p, path, value)
		case "REMOVE":
			// No directory-owned scalar we persist is removable in v1; ignore
			// unknown removes rather than error, per SCIM leniency for out-of-scope.
		default:
			return nil, errf("unsupported PATCH op %q", opType)
		}
	}
	return p, nil
}

func applyPatchValue(p *userPatch, path string, value any) {
	// path may be "active", "emails", "userName", or empty with a value object.
	lower := strings.ToLower(strings.TrimSpace(path))
	switch lower {
	case "", "emails":
		// value may be a string (email) or map
		if s, ok := value.(string); ok && s != "" {
			p.Email = s
			p.Attributes = append(p.Attributes, attrChange{Name: "emails", Value: s})
		}
	case "username", "user_name":
		if s, ok := value.(string); ok {
			p.Email = s
			p.Attributes = append(p.Attributes, attrChange{Name: "userName", Value: s})
		}
	case "active":
		if b, ok := value.(bool); ok {
			p.Active = &b
			p.Attributes = append(p.Attributes, attrChange{Name: "active", Value: b})
		}
	}
}

// ── filter parsing ─────────────────────────────────────────────────────────────

var eqFilterRe = regexp.MustCompile(`^\s*(\w+)\s+eq\s+"([^"]*)"\s*$`)

// parseEqFilter parses the minimal SCIM filter grammar supported in v1: a single
// `attr eq "value"` expression on userName or externalId. Returns (attr, value,
// ok). Unsupported grammar yields ok=false.
func parseEqFilter(filter string) (string, string, bool) {
	m := eqFilterRe.FindStringSubmatch(filter)
	if m == nil {
		return "", "", false
	}
	attr := strings.ToLower(m[1])
	switch attr {
	case "username", "externalid":
		return attr, m[2], true
	default:
		return "", "", false
	}
}

// ── discovery endpoints (read-only) ────────────────────────────────────────────

func handleServiceProviderConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"schemas":          []string{"urn:ietf:params:scim:schemas:core:2.0:ServiceProviderConfig"},
		"documentationUri": "https://example.com/scim",
		"patch":            map[string]any{"supported": true},
		"bulk":             map[string]any{"supported": false},
		"filter":           map[string]any{"supported": true, "maxResults": 200},
		"changePassword":   map[string]any{"supported": false},
		"sort":             map[string]any{"supported": false},
		"etag":             map[string]any{"supported": false},
		"authenticationSchemes": []map[string]any{{
			"name":          "OAuth Bearer Token",
			"description":   "SCIM bearer token (HMAC-bound per workspace+connection)",
			"specUri":       "http://www.rfc-editor.org/info/rfc6750",
			"documentationUri": "https://example.com/scim",
			"type":          "oauthbearertoken",
			"primary":       true,
		}},
	})
}

func handleResourceTypes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"schemas":      []string{"urn:ietf:params:scim:api:messages:listResponse"},
		"totalResults": 1,
		"Resources": []map[string]any{{
			"id":          "User",
			"name":        "User",
			"endpoint":    "/scim/v2/Users",
			"description": "SCIM 2.0 User resource",
			"schema":      userSchema,
		}},
	})
}

func handleSchemas(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"schemas":      []string{"urn:ietf:params:scim:api:messages:listResponse"},
		"totalResults": 1,
		"Resources": []map[string]any{{
			"id":         userSchema,
			"name":       "User",
			"attributes": []map[string]any{},
		}},
	})
}

// ensure idp import is used (connection status constants referenced by guards).

