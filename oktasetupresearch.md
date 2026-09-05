# Okta SCIM Push — Research: Capturing the Real Payload & Fixing the Controller

Companion to `oktasetup.md`. This documents the **debugging methodology** used to
root-cause two Okta→Zecurity SCIM "Push Groups" failures during Sprint 17
(ADR-025), and the exact controller fixes. The goal: show *how to find Okta's
actual payload* and *how to translate that into a code fix* — not just the diff.

All steps below were executed against the live system (local controller on
`:8080`, live Okta tenant `trial-3724025`, admin UI on `:5173`, driven via
`orca-ide` browser automation).

---

## 0. Symptom

Okta "Push Groups" for the `hermes` group failed with:

```
Failed: Error while creating user group hermes: Bad Request.
Errors reported by remote server: group PATCH requires a members path
or a members value object
```

And later, after the Group-PATCH parse was fixed, the admin **Groups dashboard
showed "No groups yet"** even though the `hermes` group existed in the DB with 2
members.

---

## 1. How to find Okta's ACTUAL payload (the key step)

Okta's error text is generic. **Never infer the payload shape from the SCIM RFC
or from Okta docs** — the SCIM 2.0 Test App sends surprising shapes. Capture it.

### 1.1 Temporary debug log in the SCIM PATCH handler

`controller/internal/scim/users.go` → `handlePatch`:

```go
if err := decodeJSON(r, &body); err != nil {
    writeSCIMError(w, newSCIMError(400, "invalidValue", "invalid SCIM PATCH: "+err.Error()))
    return
}
fmt.Printf("DEBUG scim handlePatch ops=%+v\n", body.Ops) // ← TEMP, remove after
patch, perr := patchGroupFromOps(body.Ops)
```

(`"fmt"` must be imported; revert both after capture.)

### 1.2 Restart the controller so the log compiles in

```bash
cd controller && export $(grep -vE '^\s*#|^\s*$' .env | xargs)
fuser -k 8080/tcp 9090/tcp 2>/dev/null; sleep 3   # kill any stale go run
go run ./cmd/server/main.go                        # run in background
```

> NOTE: a `go run` child process can linger and keep `:8080` bound, so a second
> `go run` fails with "address already in use". Always `fuser -k` the ports and
> confirm a single listener (`ss -ltnp | grep :8080`) before retrying.

### 1.3 Trigger the push and read the log

In the Okta admin UI (via `orca-ide`): Push Groups → "Retry All Groups". Then read
the controller's stdout. The captured line is ground truth:

```
DEBUG scim handlePatch ops=[map[op:replace value:map[displayName:hermes id:e01f84ad-56a4-40d8-b17c-6096c8da83d2]]]
```

**Okta actually sent** a group-metadata PATCH:

```json
{ "op": "replace", "value": { "displayName": "hermes", "id": "e01f84ad-…" } }
```

— a full resource object with **no `members` key**. That is what the old parser
rejected with "group PATCH requires a members path or a members value object".

---

## 2. Root cause A — members-less resource PATCH → 400

`controller/internal/scim/groups.go`, `groupMemberValues`:

The old code handled `path:"members"` (string) and `value:{"members":[…]}` (map
with members key), but when `normed==""` (no path) and `value` was a map **without**
a `"members"` key — or a bare `[]any` array — it fell through to the 400 error.

Okta sends two shapes the old code missed:
1. `{"op":"replace","value":{"displayName":…,"id":…}}` — metadata sync, no members.
2. `{"op":"add","value":[{"value":"<id>"},…]}` — first-push bare member array.

### 2.1 Fix

```go
if normed == "" {
    if obj, ok := value.(map[string]any); ok {
        if mem, ok := obj["members"]; ok {
            return groupMemberValues(opType, "members", mem)
        }
        // Resource object without a members key → no membership change (no-op).
        return nil, nil
    }
    if arr, ok := value.([]any); ok {
        return groupMemberValues(opType, "members", arr)
    }
    return nil, fmt.Errorf("group PATCH requires a members path or a members value object")
}
```

A members-less resource object is now a **no-op** (not an error). The bare
`[]any` member array is parsed directly.

### 2.2 Test (reproduce the EXACT captured payload)

`controller/internal/scim/groups_integration_test.go` — new case in
`TestGroups_HTTP`:

```go
rec = do("PATCH", "/scim/v2/Groups/"+bare.ID,
    `{"schemas":["urn:ietf:params:scim:api:messages:patchOp"],"Operations":`+
    `[{"op":"replace","value":{"displayName":"okta-bare-array","id":"`+bare.ID+`"}}]}`)
if rec.Code != http.StatusOK {
    t.Fatalf("Okta metadata-replace PATCH (no members): expected 200, got %d body=%s", rec.Code, rec.Body.String())
}
```

---

## 3. Root cause B — "No groups yet" → GetGroup scan/SELECT mismatch

After fixing A, the push succeeded (Okta: "All errors resolved"; DB confirmed
`hermes` had 2 members). But the **admin Groups dashboard still showed "No groups
yet"**.

### 3.1 Don't guess — probe the live GraphQL through the browser session

Use `orca-ide eval` to run an authenticated `groups` query *through the admin
UI's own session* (so it carries the real identity). The full recipe is in the
orca-cli skill reference `references/authenticated-graphql-probe.md`. The one-shot
JS (run via `orca-ide eval --page <pageId> --expression "$(cat q.js)"`):

```js
(async () => {
  const mod = await import('/src/store/auth.ts');
  const token = mod.useAuthStore.getState().accessToken;
  const payload = JSON.parse(atob(token.split('.')[1]));
  const r = await fetch('/graphql', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + token },
    body: JSON.stringify({ query: '{ groups { id name origin externalId members { email } } }' })
  });
  const j = await r.json();
  return JSON.stringify({ decoded_tenant: payload.tenant_id, groups: j }).slice(0, 2500);
})()
```

The result **simultaneously** revealed:
- `decoded_tenant = b95782b1-…` — exactly the workspace the `hermes` group lives
  in → **tenant-scoping was NOT the problem**.
- Server error: `"loadGroup: get group: number of field descriptions must equal
  number of destinations, got 9 and 6"` → a **controller code defect**, not data.

### 3.2 The defect

`controller/internal/policy/store.go`, `GetGroup` selected 9 columns but scanned 6:

```go
// BEFORE (broken)
`SELECT id, workspace_id, name, description, created_at, updated_at,
        origin, external_id, connection_id
 FROM groups WHERE id = $1`, id,
).Scan(&row.ID, &row.WorkspaceID, &row.Name, &row.Description, &row.CreatedAt, &row.UpdatedAt)
//        ^ only 6 destinations for 9 columns → pgx panics
```

The sibling `ListGroups` already scanned all 9 (including
`&r.Origin, &r.ExternalID, &r.ConnectionID`) — `GetGroup` was the copy-paste
that drifted. Because `loadGroup` calls `GetGroup` for *every* group, the entire
`groups` query failed → dashboard empty.

### 3.3 Fix

```go
).Scan(&row.ID, &row.WorkspaceID, &row.Name, &row.Description, &row.CreatedAt, &row.UpdatedAt,
    &row.Origin, &row.ExternalID, &row.ConnectionID)
```

### 3.4 Verify

Re-run the live `groups` query via the browser probe → now returns:

```json
{ "id": "e01f84ad-…", "name": "hermes", "origin": "scim", "externalId": "hermes",
  "members": [ {"email":"sathiyaseelank326@gmail.com"},
               {"email":"xiyo@gomail.edu.pl"} ] }
```

Reload the admin Groups page → renders `hermes · SCIM (okta scim)`, 2 members.

---

## 4. End-to-end verification checklist

- [ ] `go build ./...` and `go vet ./internal/scim/... ./internal/policy/...` pass.
- [ ] Unit test reproducing the **exact** captured Okta payload passes
      (`go test ./internal/scim/... -run TestGroups_HTTP`).
- [ ] Temporary `fmt.Printf` debug log **removed** before commit.
- [ ] Controller restarted (single `:8080` listener confirmed).
- [ ] Live `groups` GraphQL query through the **browser session** returns the
      group (not just "no error").
- [ ] Admin dashboard renders the synced group with correct member count.
- [ ] DB cross-check: `hermes` group has the expected members under its workspace.

---

## 5. Replacing the Okta endpoint (when the tunnel is down)

If Okta can't reach the controller (no Cloudflare tunnel), the SCIM Base Url is
stale:

1. Create a quick tunnel: `cloudflared tunnel --url http://localhost:8080` → note
   the `https://<rand>.trycloudflare.com` URL.
2. In Okta admin (via `orca-ide`): SCIM app → **Provisioning** → **Integration**
   → **Edit** → set **SCIM 2.0 Base Url** to
   `https://<rand>.trycloudflare.com/scim/v2` → **Save**.
3. Retry the group push.

> Quick tunnels are ephemeral: restarting `cloudflared` changes the URL, so Okta's
> Base Url must be updated again. For stable use, set up a **named** Cloudflare
> tunnel with a cert so the URL is constant.

---

## 6. Standing rules (Sprint 17 / ADR-025)

- Do **not** commit implementation code unless explicitly told. Fixes stay
  uncommitted for review.
- Capture the real payload; never fabricate an Okta request shape.
- Verify with a unit test reproducing the exact payload AND a live browser-session
  GraphQL query — a no-error UI reload is not proof.
