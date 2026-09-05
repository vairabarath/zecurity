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
	mux.HandleFunc("DELETE /scim/v2/Users/{id}", users.handleDelete)

	groups := &groupHandler{store: s, ds: ds}
	mux.HandleFunc("POST /scim/v2/Groups", groups.handlePost)
	mux.HandleFunc("GET /scim/v2/Groups", groups.handleList)
	mux.HandleFunc("GET /scim/v2/Groups/{id}", groups.handleGet)
	mux.HandleFunc("PUT /scim/v2/Groups/{id}", groups.handlePut)
	mux.HandleFunc("PATCH /scim/v2/Groups/{id}", groups.handlePatch)
	mux.HandleFunc("DELETE /scim/v2/Groups/{id}", groups.handleDelete)

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

// syncInstanceFor ensures a sync instance exists for the connection (opening a
// new one on first write) and returns its id. Every SCIM write carries the
// connection's current sync instance so provisioned objects record provenance
// and a disable→re-enable reconnect can reconcile stale-vs-current (ADR-025 §12).
func (h *userHandler) syncInstanceFor(ctx context.Context, sc *scope) string {
	inst, err := h.ds.EnsureSyncInstance(ctx, sc)
	if err != nil {
		// Best-effort: fall back to "" (NULL sync_instance_id) rather than
		// failing the whole write.
		return ""
	}
	return inst
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
	if serr := h.dispatchActive(r.Context(), sc, id, patch); serr != nil {
		writeSCIMError(w, serr)
		return
	}
	if patch.Active == nil {
		// Non-active PUT is a pure attribute update.
		u, serr := h.ds.Update(r.Context(), sc, id, patch, h.syncInstanceFor(r.Context(), sc))
		if serr != nil {
			writeSCIMError(w, serr)
			return
		}
		writeUser(w, *u)
		return
	}
	writeUser(w, User{ID: id, Schemas: []string{userSchema}})
}

func (h *userHandler) handlePatch(w http.ResponseWriter, r *http.Request) {
	sc, serr := h.scope(r.Context())
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	id := r.PathValue("id")
	var body struct {
		Schemas []string         `json:"schemas"`
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
	if serr := h.dispatchActive(r.Context(), sc, id, patch); serr != nil {
		writeSCIMError(w, serr)
		return
	}
	if patch.Active == nil {
		u, serr := h.ds.Update(r.Context(), sc, id, patch, h.syncInstanceFor(r.Context(), sc))
		if serr != nil {
			writeSCIMError(w, serr)
			return
		}
		writeUser(w, *u)
		return
	}
	writeUser(w, User{ID: id, Schemas: []string{userSchema}})
}

// dispatchActive routes an active-state change (PATCH/PUT active=true|false) to
// the transactional Deprovision/Reactivate path (which enqueues the
// device-trust event). When Active is nil the caller falls through to Update.
func (h *userHandler) dispatchActive(ctx context.Context, sc *scope, id string, patch *userPatch) *SCIMError {
	if patch.Active == nil {
		return nil
	}
	if *patch.Active {
		return h.ds.Reactivate(ctx, sc, id, h.syncInstanceFor(ctx, sc))
	}
	return h.ds.Deprovision(ctx, sc, id, false, h.syncInstanceFor(ctx, sc))
}

func (h *userHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	sc, serr := h.scope(r.Context())
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	id := r.PathValue("id")
	// Default DELETE is a reversible soft-delete (status='deleted' tombstone).
	// ?hard=true makes it a permanent tombstone (still soft-delete at the DB
	// layer; the distinction is the audit reason + ADR-025 §9 semantics).
	hard := r.URL.Query().Get("hard") == "true"
	if serr := h.ds.Deprovision(r.Context(), sc, id, hard, h.syncInstanceFor(r.Context(), sc)); serr != nil {
		writeSCIMError(w, serr)
		return
	}
	// RFC 7644 §3.2: a successful DELETE returns 204 No Content with an empty
	// body. Returning a 404 error envelope here would be wrong — the operation
	// succeeded; the resource is now gone (soft-deleted tombstone).
	w.WriteHeader(http.StatusNoContent)
}

// ── request parsing ───────────────────────────────────────────────────────────

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return jsonDecode(r.Body, v)
}

// userPatch is the normalized set of directory-owned mutations Phase 5 may apply.
// Unsupported attributes are rejected earlier (supportedDirectoryAttr).
type userPatch struct {
	Email  string
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
			"name":             "OAuth Bearer Token",
			"description":      "SCIM bearer token (HMAC-bound per workspace+connection)",
			"specUri":          "http://www.rfc-editor.org/info/rfc6750",
			"documentationUri": "https://example.com/scim",
			"type":             "oauthbearertoken",
			"primary":          true,
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

// ── Group handler (Phase 7) ────────────────────────────────────────────────────

// groupHandler dispatches SCIM Group operations, extracting scope from the
// authenticated token and delegating to DirectoryService. Scope is bound from
// the token only — never from the request payload.
type groupHandler struct {
	store *Store
	ds    *DirectoryService
}

func (h *groupHandler) scope(ctx context.Context) (*scope, *SCIMError) {
	tok := TokenFromContext(ctx)
	if tok == nil {
		return nil, newSCIMError(401, "", "missing SCIM token")
	}
	return h.ds.resolveScope(ctx, tok.WorkspaceID, tok.ConnectionID)
}

func (h *groupHandler) handlePost(w http.ResponseWriter, r *http.Request) {
	sc, serr := h.scope(r.Context())
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	var resource map[string]any
	if err := decodeJSON(r, &resource); err != nil {
		writeSCIMError(w, newSCIMError(400, "invalidValue", "invalid SCIM Group resource: "+err.Error()))
		return
	}
	externalID := strTrim(resource["externalId"])
	displayName := strTrim(resource["displayName"])
	// externalId stays the preferred Canonical Identity Key. When the IdP omits
	// it (Okta "Push Groups" by name sends displayName only), CreateGroup
	// derives a slug fallback via DeriveGroupExternalID — see the caveats on
	// that function. Test the derivation, not just displayName != "", so this
	// guard and CreateGroup's own fallback can never disagree (e.g. "!!!"
	// is non-empty but derives to "").
	if externalID == "" && DeriveGroupExternalID(displayName) == "" {
		writeSCIMError(w, newSCIMError(400, "invalidValue",
			"externalId is required (or a displayName from which one can be derived)"))
		return
	}
	g, serr := h.ds.CreateGroup(r.Context(), sc, externalID, displayName)
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	writeGroupCreated(w, g)
}

func (h *groupHandler) handleList(w http.ResponseWriter, r *http.Request) {
	sc, serr := h.scope(r.Context())
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	groups, err := h.ds.ListGroups(r.Context(), sc)
	if err != nil {
		writeSCIMError(w, newSCIMError(500, "", "list groups: "+err.Error()))
		return
	}
	writeGroupList(w, groups)
}

func (h *groupHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	sc, serr := h.scope(r.Context())
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	id := r.PathValue("id")
	g, serr := h.ds.GetGroup(r.Context(), sc, id)
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	members, _ := h.ds.ListGroupMembersAsMembers(r.Context(), sc, g.ID)
	writeGroup(w, g, members)
}

func (h *groupHandler) handlePut(w http.ResponseWriter, r *http.Request) {
	sc, serr := h.scope(r.Context())
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	id := r.PathValue("id")
	var resource map[string]any
	if err := decodeJSON(r, &resource); err != nil {
		writeSCIMError(w, newSCIMError(400, "invalidValue", "invalid SCIM Group resource: "+err.Error()))
		return
	}
	displayName := strTrim(resource["displayName"])

	// members may be absent (name-only PUT) or present (full replacement).
	var members []memberChange
	if raw, ok := resource["members"].([]any); ok {
		for _, item := range raw {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			v, _ := obj["value"].(string)
			if v == "" {
				if ref, _ := obj["$ref"].(string); ref != "" {
					v = ref
				}
			}
			if v != "" {
				members = append(members, memberChange{Op: "replace", Value: v})
			}
		}
	}

	g, serr := h.ds.ReplaceGroup(r.Context(), sc, id, displayName, members)
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	mem, _ := h.ds.ListGroupMembersAsMembers(r.Context(), sc, g.ID)
	writeGroup(w, g, mem)
}

func (h *groupHandler) handlePatch(w http.ResponseWriter, r *http.Request) {
	sc, serr := h.scope(r.Context())
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	id := r.PathValue("id")
	var body struct {
		Schemas []string         `json:"schemas"`
		Ops     []map[string]any `json:"Operations"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeSCIMError(w, newSCIMError(400, "invalidValue", "invalid SCIM PATCH: "+err.Error()))
		return
	}
	patch, perr := patchGroupFromOps(body.Ops)
	if perr != nil {
		writeSCIMError(w, newSCIMError(400, "invalidValue", perr.Error()))
		return
	}
	g, serr := h.ds.PatchGroup(r.Context(), sc, id, patch)
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	members, _ := h.ds.ListGroupMembersAsMembers(r.Context(), sc, g.ID)
	writeGroup(w, g, members)
}

func (h *groupHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	sc, serr := h.scope(r.Context())
	if serr != nil {
		writeSCIMError(w, serr)
		return
	}
	id := r.PathValue("id")
	if serr := h.ds.DeleteGroup(r.Context(), sc, id); serr != nil {
		writeSCIMError(w, serr)
		return
	}
	// RFC 7644 §3.2: successful DELETE → 204 No Content, empty body.
	w.WriteHeader(http.StatusNoContent)
}

// ── Group response helpers ───────────────────────────────────────────────────

func writeGroup(w http.ResponseWriter, g *groupRow, members []Member) {
	writeJSON(w, http.StatusOK, groupToResource(g, members))
}

func writeGroupCreated(w http.ResponseWriter, g *groupRow) {
	w.Header().Set("Location", "/scim/v2/Groups/"+g.ID)
	writeJSON(w, http.StatusCreated, groupToResource(g, nil))
}

func groupToResource(g *groupRow, members []Member) Group {
	return Group{
		Schemas:     []string{groupSchema},
		ID:          g.ID,
		DisplayName: g.Name,
		ExternalID:  g.ExternalID,
		Members:     members,
		Meta: Meta{
			ResourceType: "Group",
			LastModified: g.UpdatedAt,
			Location:     "/scim/v2/Groups/" + g.ID,
		},
	}
}

func writeGroupList(w http.ResponseWriter, groups []groupRow) {
	resources := make([]Group, 0, len(groups))
	for i := range groups {
		resources = append(resources, Group{
			Schemas:     []string{groupSchema},
			ID:          groups[i].ID,
			DisplayName: groups[i].Name,
			ExternalID:  groups[i].ExternalID,
			Meta: Meta{
				ResourceType: "Group",
				LastModified: groups[i].UpdatedAt,
				Location:     "/scim/v2/Groups/" + groups[i].ID,
			},
		})
	}
	writeJSON(w, http.StatusOK, ListResponse{
		Schemas:      []string{"urn:ietf:params:scim:api:messages:listResponse"},
		TotalResults: len(resources),
		StartIndex:   1,
		ItemsPerPage: len(resources),
		Resources:    toAnySlice(resources),
	})
}

func strTrim(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}
