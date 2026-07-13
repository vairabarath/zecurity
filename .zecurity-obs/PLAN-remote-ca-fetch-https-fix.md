---
type: fix-plan
scope: connector, shield
status: draft
date: 2026-07-07
branch: integration/relay-merge
---

# Fix: connector/shield CA bootstrap fails over a public WAN link

## Context

We just deployed the controller on a VPS at `controller.zecurity.in`, fronted by nginx
(HTTPS on 443 → plaintext on localhost:8080). The relay is on the *same* box, so it
fetches `/ca.crt` via plaintext `http://127.0.0.1:8080` — that's safe and correct because
it never leaves the loopback interface.

The **connector** and **shield** are on a separate office LAN. Their enrollment code does
the exact same plaintext `http://{CONTROLLER_HTTP_ADDR}/ca.crt` fetch
(`connector/src/enrollment.rs:320`, `shield/src/enrollment.rs:303`), but hardcodes the
`http://` scheme with no way to opt into HTTPS. Pointed at a remote controller, that fetch
has nowhere plaintext to land — the VPS only exposes the raw HTTP port on localhost; the
public path is HTTPS-only on 443 via nginx.

We rejected the alternative (temporarily opening port 8080 to the internet) because it
exposes the controller's *entire* plaintext HTTP surface (GraphQL, OAuth, invitations),
not just `/ca.crt`, and "temporary" firewall rules tend to outlive the test. This plan
fixes the actual constraint instead: teach `fetch_ca_cert` to respect an explicit scheme,
so `CONTROLLER_HTTP_ADDR` can be a real HTTPS URL. This mirrors the convention the
**client** already uses correctly (`client/src/config.rs:36-44`, `http_base_url` — used
verbatim, whatever scheme it carries) — we're just bringing connector and shield in line
with it.

Fingerprint verification (`verify_ca_fingerprint`, checked against the enrollment JWT's
embedded hash) is unaffected either way and stays the actual security guarantee — this
fix is about making the fetch reachable at all, not about the trust model.

## Change 1 — `connector/src/enrollment.rs`

Current (`enrollment.rs:314-320`):
```rust
async fn fetch_ca_cert(http_addr: &str) -> Result<String> {
    let client = Client::builder()
        .build()
        .context("failed to build HTTP client")?;

    let url = format!("http://{}/ca.crt", http_addr);
```

Change to:
```rust
async fn fetch_ca_cert(http_addr: &str) -> Result<String> {
    let client = Client::builder()
        .build()
        .context("failed to build HTTP client")?;

    let url = ca_cert_url(http_addr);
```

Add a small helper near the top of the "Internal helpers" section (after the
`parse_jwt_payload` function is fine):
```rust
/// Builds the /ca.crt URL from a configured HTTP address.
///
/// `http_addr` may be a bare "host:port" (assumed http://, for co-located/dev use —
/// e.g. the relay-style "127.0.0.1:8080" pattern) or a full URL with an explicit
/// "http://" / "https://" scheme (required when the controller is on a different
/// network and only reachable via its public HTTPS endpoint).
fn ca_cert_url(http_addr: &str) -> String {
    if http_addr.starts_with("http://") || http_addr.starts_with("https://") {
        format!("{}/ca.crt", http_addr.trim_end_matches('/'))
    } else {
        format!("http://{}/ca.crt", http_addr)
    }
}
```

No other change needed in this file — `derive_http_addr` (`enrollment.rs:410-417`, used
only when `CONTROLLER_HTTP_ADDR` is unset) keeps producing a bare `host:8080`, which
`ca_cert_url` still treats as `http://`, so co-located/dev behavior is unchanged.

**Bonus consistency note (no code change required):** `main.rs:150-162`'s CRL URL
construction already uses `cfg.controller_http_addr` verbatim without prepending a scheme
— it only hardcodes `http://` in its own *fallback* branch (when the var is unset). Once
we set `CONTROLLER_HTTP_ADDR=https://controller.zecurity.in` explicitly in the office
deployment, the periodic CRL fetch (`main.rs:168`, every 300s) will automatically pick up
HTTPS too, with no further changes.

## Change 2 — `shield/src/enrollment.rs`

Same fix, same reasoning. Current (`enrollment.rs:302-303`):
```rust
async fn fetch_ca_cert(http_addr: &str) -> Result<String> {
    let url = format!("http://{}/ca.crt", http_addr);
```

Change to:
```rust
async fn fetch_ca_cert(http_addr: &str) -> Result<String> {
    let url = ca_cert_url(http_addr);
```

Add the identical `ca_cert_url` helper (shield and connector are separate crates with no
shared lib today — this file already duplicates the whole enrollment flow independently,
so a duplicated 7-line helper matches the existing pattern rather than introducing a new
shared dependency for one function).

## Change 3 — doc comments (clarity only, no behavior change)

Update these two doc comments to describe the new convention:

- `connector/src/config.rs:60-63` (the `controller_http_addr` field doc)
- `shield/src/config.rs:50-52` (the `controller_http_addr` field doc)

Both currently read like `Example: "controller.example.com:8080"`. Extend to:
```
/// Example: "controller.example.com:8080" (assumes http://) or
/// "https://controller.example.com" (explicit scheme — required when the
/// controller is only reachable over HTTPS, e.g. a remote/WAN deployment).
```

## Change 4 — install scripts also do their own plaintext curl

Each install script independently fetches `/ca.crt` *before* the binary ever runs (a
separate, non-fingerprint-checked bootstrap step — just used to pre-seed
`${CONFIG_DIR}/ca.crt`). These hardcode `http://` too and need the same scheme-awareness:

- `scripts/connector-local-install.sh:83-84`
- `connector/scripts/connector-install.sh:143-144`
- `shield/scripts/shield-install.sh:203-204`

Current pattern (identical shape in all three):
```bash
log "fetching /ca.crt from http://${CONTROLLER_HTTP_ADDR}"
if ! curl -fsSL --max-time 10 "http://${CONTROLLER_HTTP_ADDR}/ca.crt" -o "${CONFIG_DIR}/ca.crt"; then
    err "failed to fetch /ca.crt from controller"
fi
```

Replace with (bash pattern-match for an existing scheme, default to `http://`):
```bash
case "${CONTROLLER_HTTP_ADDR}" in
    http://*|https://*) CA_URL="${CONTROLLER_HTTP_ADDR%/}/ca.crt" ;;
    *)                  CA_URL="http://${CONTROLLER_HTTP_ADDR}/ca.crt" ;;
esac
log "fetching /ca.crt from ${CA_URL}"
if ! curl -fsSL --max-time 10 "${CA_URL}" -o "${CONFIG_DIR}/ca.crt"; then
    err "failed to fetch /ca.crt from controller"
fi
```
(adjust `--max-time` to match each script's existing value — 10s for the local-install
script, 30s for the other two).

Also update each script's usage/help text (`connector-local-install.sh:15,45`,
`connector-install.sh:15,64`, `shield-install.sh:15,51`) to note
`CONTROLLER_HTTP_ADDR` accepts either a bare `host:port` or a full URL with scheme.

## Build & redeploy (do this on your local machine, then push to the office LAN devices)

1. Apply changes 1–4 above.
2. `cd connector && cargo build --release` and `cd shield && cargo build --release`
   (or `cargo build --release --manifest-path connector/Cargo.toml` /
   `--manifest-path shield/Cargo.toml` from repo root).
3. Run each crate's existing test suite (`cargo test`) — worth adding a unit test for
   `ca_cert_url` in both files (bare host:port → `http://` prefix; already-schemed input
   passed through unchanged) since this is real logic now, not just a format string.
4. Copy the two new binaries to the LAN connector/shield hosts (`scp` or however you
   currently deploy), overwriting `/usr/local/bin/zecurity-connector` and
   `/usr/local/bin/zecurity-shield`.
5. Commit and push this branch; I'll `git pull` on the VPS side (nothing there needs the
   fix — the controller doesn't call `fetch_ca_cert`).

## Re-enrolling connector + shield against the new controller

This VPS's controller database is currently empty — no workspaces, no connectors. So this
isn't just a config edit; it's a fresh enrollment against a new controller instance:

1. Log into `https://admin.zecurity.in` (Google OAuth) and create/confirm the workspace.
2. From the admin UI, generate a new connector enrollment token (and a shield token once
   the connector exists), or via the `POST /api/relays`-style GraphQL mutations
   (`generateConnectorToken`, presumably an analogous `generateShieldToken`).
3. On the connector host: stop the service, remove the stale state
   (`/var/lib/zecurity-connector/state.json`, `connector.crt`, `workspace_ca.crt`,
   `connector.key` — these were signed by whatever controller/CA this connector was
   enrolled against before, which is not this one), update `/etc/zecurity/connector.conf`:
   ```
   CONTROLLER_ADDR=controller.zecurity.in:9090
   CONTROLLER_HTTP_ADDR=https://controller.zecurity.in
   ENROLLMENT_TOKEN=<new token>
   ```
   then `systemctl restart zecurity-connector` and confirm via
   `journalctl -u zecurity-connector -f` (expect the same
   "CA fingerprint verified" / "Enroll" success lines we saw on the relay).
4. Same for shield: wipe its state dir, update `/etc/zecurity/shield.conf` with the new
   `CONTROLLER_ADDR` / `CONTROLLER_HTTP_ADDR` / a fresh shield enrollment token, restart.
   Recall shield only needs the controller address for this one-time enrollment — after
   that it talks exclusively to its connector on `:9091`.
5. Client: `zecurity-client setup --workspace <ws> --controller controller.zecurity.in:9090 --http-base https://controller.zecurity.in`, then restart the client daemon and
   `zecurity-client login`.

## Verification

- `journalctl -u zecurity-connector -n 40` and `-u zecurity-shield -n 40` on the LAN boxes
  show successful CA fetch + fingerprint verification + Enroll, same shape as the relay's
  log lines we already confirmed on the VPS.
- On the VPS: `sudo docker exec ztna_postgres psql -U ztna -d ztna_platform -c "SELECT id,name,status FROM connectors;"` and the equivalent for `shields` show new rows.
- `zecurity-client status` shows connected, and `zecurity-client resources` lists whatever
  resources the connector has published — confirms the full client → relay → connector
  path the original WAN test set out to validate.
