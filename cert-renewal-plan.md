# Cert Renewal Sprint Plan
## Solo Implementation — You Are Doing This Yourself Today

---

## The Big Picture (Read This First)

Right now your connector has a 7-day cert. When it expires, the connector
goes DISCONNECTED permanently — it can't heartbeat anymore because the
cert it uses for mTLS is dead.

You need to make this happen automatically instead:

```
Day 1   Connector enrolled  →  7-day cert issued
Day 5   Heartbeat happens   →  Controller: "hey your cert expires in 2 days, here's a fresh one"
Day 5   Connector receives  →  saves new cert, keeps heartbeating without interruption
Day 8   Old cert would have expired  →  doesn't matter, connector already has a new one
```

The connector never goes offline. The admin never has to do anything.
It just works silently in the background forever.

---

## Why This Is Easier Than You Think

Everything you need already exists:

```
✅ re_enroll field already in HeartbeatResponse proto (always false right now)
✅ cert_not_after already stored in the connectors DB table
✅ SignConnectorCert() already exists in pki/workspace.go
✅ mTLS already authenticates the connector on every heartbeat
✅ SPIFFE interceptor already extracts connector identity from the cert
✅ state.json already stores cert_not_after on the Rust side
✅ connector.crt + connector.key already on disk
```

You are not building anything from scratch.
You are connecting dots that are already drawn.

---

## What You Are Building

Three things. That's it.

```
1. Controller  →  detect "cert expiring soon" → call RenewCert RPC → issue fresh cert
2. Proto       →  add RenewCert RPC + messages (5 lines)
3. Connector   →  when heartbeat says re_enroll=true → call RenewCert → save new cert
```

No new JWT. No admin action. No install script. No Redis. No new DB columns.
The mTLS cert the connector already has IS the proof of identity.

---

## How The Renewal Flow Works Step by Step

```
Every 30 seconds the connector sends a heartbeat (this already works).

                    Connector                        Controller
                       │                                │
                       │  HeartbeatRequest (mTLS)       │
                       │ ──────────────────────────────>│
                       │                                │ 1. SPIFFE interceptor extracts
                       │                                │    connectorID from cert
                       │                                │ 2. Updates last_heartbeat_at
                       │                                │ 3. Checks cert_not_after:
                       │                                │    now + 48h > cert_not_after?
                       │                                │    NO  → re_enroll = false (normal)
                       │                                │    YES → re_enroll = true
                       │  HeartbeatResponse             │
                       │ <──────────────────────────────│
                       │  { ok, re_enroll: true }       │
                       │                                │
  re_enroll=true?      │                                │
  YES → call RenewCert │                                │
                       │  RenewCertRequest (mTLS)       │
                       │ ──────────────────────────────>│
                       │  { connector_id (log only) }   │ 4. SPIFFE interceptor extracts
                       │                                │    identity again (same mTLS cert)
                       │                                │ 5. Calls SignConnectorCert()
                       │                                │    → fresh 7-day cert, same SPIFFE ID
                       │                                │ 6. Updates cert_not_after in DB
                       │  RenewCertResponse             │
                       │ <──────────────────────────────│
                       │  { certificate_pem,            │
                       │    workspace_ca_pem,           │
                       │    intermediate_ca_pem }       │
                       │                                │
  Save new cert        │                                │
  to connector.crt     │                                │
  Update state.json    │                                │
  Keep heartbeating    │                                │
  with new cert        │                                │
```

The old cert is still valid when this happens (2 days left on it).
The new cert becomes active immediately on the next heartbeat connection.
Zero downtime. Zero admin action.

---

## .env Addition

One new variable. Add to controller `.env`:

```env
# How early to start renewing — 48h means "renew when < 48h left on cert"
# With 7-day certs: renewal starts on day 5
CONNECTOR_RENEWAL_WINDOW=48h
```

Add to `ConnectorConfig` in `internal/connector/config.go`:

```go
// RenewalWindow is how far before expiry the controller starts
// returning re_enroll=true in heartbeat responses.
// Env: CONNECTOR_RENEWAL_WINDOW (default: 48h)
RenewalWindow time.Duration
```

Add to `main.go` alongside existing mustDuration calls:

```go
RenewalWindow: mustDuration("CONNECTOR_RENEWAL_WINDOW", 48*time.Hour),
```

---

## Step 1 — Update the Proto (connector/proto/connector.proto)

Add one new RPC and two new messages.
The existing Enroll and Heartbeat messages are UNCHANGED.

```protobuf
service ConnectorService {
  rpc Enroll(EnrollRequest) returns (EnrollResponse);
  rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse);

  // NEW — called by connector when heartbeat returns re_enroll=true.
  // Uses mTLS — connector presents its current (still-valid) cert.
  // No JWT needed. The mTLS cert IS the proof of identity.
  // Controller issues a fresh 7-day cert for the same SPIFFE ID.
  rpc RenewCert(RenewCertRequest) returns (RenewCertResponse);
}

// NEW
message RenewCertRequest {
  string connector_id = 1;  // for logging only — identity comes from mTLS cert
}

// NEW
message RenewCertResponse {
  bytes certificate_pem     = 1;  // fresh 7-day cert, same SPIFFE SAN
  bytes workspace_ca_pem    = 2;  // WorkspaceCA (may not have changed, but send anyway)
  bytes intermediate_ca_pem = 3;  // Intermediate CA
}
```

After editing the proto:

**Go side** — run protoc to regenerate stubs:
```bash
cd controller
protoc --go_out=. --go-grpc_out=. proto/connector.proto
```

**Rust side** — `build.rs` already calls `tonic_build::compile_protos` so it
regenerates automatically on `cargo build`. Nothing extra to do.

---

## Step 2 — Heartbeat Handler (controller/internal/connector/heartbeat.go)

Find where the handler currently builds the HeartbeatResponse.
Right now it always returns `re_enroll: false`.

Change it to check cert expiry:

```go
// Current code (somewhere in your heartbeat handler):
return &pb.HeartbeatResponse{
    Ok:            true,
    LatestVersion: latestVersion,
    ReEnroll:      false,  // ← change this
}, nil

// New code:
reEnroll := false
if connector.CertNotAfter != nil {
    // If the cert expires within the renewal window, tell the connector to renew
    renewBy := time.Now().Add(s.cfg.RenewalWindow)
    if connector.CertNotAfter.Before(renewBy) {
        reEnroll = true
        tracing.Info("cert expiring soon, requesting renewal",
            "connector_id", connectorID,
            "cert_not_after", connector.CertNotAfter,
            "renewal_window", s.cfg.RenewalWindow,
        )
    }
}

return &pb.HeartbeatResponse{
    Ok:            true,
    LatestVersion: latestVersion,
    ReEnroll:      reEnroll,
}, nil
```

You need to load `connector.CertNotAfter` in the heartbeat handler.
You are probably already loading the connector row to check revocation status
(step 4 in the heartbeat flow). Just read `cert_not_after` from that same row.
No extra DB query needed.

---

## Step 3 — RenewCert Handler (controller/internal/connector/renewal.go)

New file. This is the simplest handler in the whole codebase.
The SPIFFE interceptor already does all the hard work before this runs.

```go
// internal/connector/renewal.go
package connector

// RenewCert handles the RenewCert RPC.
//
// By the time this runs, the SPIFFE interceptor has already:
//   - verified the mTLS cert
//   - validated the trust domain
//   - injected connectorID + trustDomain into context
//
// This handler just issues a fresh cert for the same identity.
// No JWT. No Redis. No new keypair. Same SPIFFE ID, fresh validity window.
func (s *service) RenewCert(
    ctx context.Context,
    req *pb.RenewCertRequest,
) (*pb.RenewCertResponse, error) {

    // 1. Read identity from context (interceptor already validated everything)
    trustDomain := ctx.Value(trustDomainKey{}).(string)
    role        := ctx.Value(spiffeRoleKey{}).(string)
    connectorID := ctx.Value(spiffeEntityIDKey{}).(string)

    // 2. Must be a connector cert (not an agent or anything else)
    if role != appmeta.SPIFFERoleConnector {
        return nil, status.Error(codes.PermissionDenied, "not a connector cert")
    }

    // 3. Load connector row — verify it's active and get tenant ID
    connector, err := s.db.GetConnectorByIDAndTrustDomain(ctx, connectorID, trustDomain)
    if err != nil || connector == nil {
        return nil, status.Error(codes.PermissionDenied, "connector not found")
    }
    if connector.Status == "revoked" {
        return nil, status.Error(codes.PermissionDenied, "connector has been revoked")
    }

    // 4. Generate a fresh cert for the same SPIFFE ID
    //    No CSR needed — controller generates the cert directly from the
    //    existing SPIFFE identity. The connector keeps its existing keypair.
    //    Same key pair + new cert = seamless renewal.
    result, err := s.pki.RenewConnectorCert(ctx,
        connector.TenantID,
        connectorID,
        trustDomain,
        s.cfg.CertTTL,   // fresh 7-day window
    )
    if err != nil {
        return nil, status.Error(codes.Internal, "failed to issue renewal cert")
    }

    // 5. Update cert_not_after in DB so heartbeat renewal window recalculates
    err = s.db.UpdateConnectorCert(ctx, connectorID, connector.TenantID,
        result.Serial, result.NotAfter)
    if err != nil {
        return nil, status.Error(codes.Internal, "failed to update cert record")
    }

    tracing.Info("connector cert renewed",
        "connector_id", connectorID,
        "trust_domain", trustDomain,
        "new_not_after", result.NotAfter,
    )

    return &pb.RenewCertResponse{
        CertificatePem:    []byte(result.CertificatePEM),
        WorkspaceCaPem:    result.WorkspaceCAPEM,
        IntermediateCaPem: result.IntermediateCAPEM,
    }, nil
}
```

---

## Step 4 — RenewConnectorCert in PKI (controller/internal/pki/workspace.go)

Add this method alongside the existing `SignConnectorCert`.
It is almost identical — the only difference is there is no CSR.
The controller generates the cert directly from the known SPIFFE ID.

```go
// RenewConnectorCert issues a fresh leaf cert for an existing connector.
//
// Unlike SignConnectorCert (enrollment), there is no CSR here.
// The connector keeps its existing EC P-384 keypair — we just
// issue a new cert wrapping the same public key + same SPIFFE ID.
//
// The public key is retrieved from the existing cert stored in the DB
// (or we re-sign with the same SPIFFE ID — the controller does not
// need to know the public key to issue a SPIFFE cert for renewal,
// since the cert is verified via the CA chain, not the public key alone).
//
// Simplest correct approach: the connector sends its current cert's
// public key inside the RenewCertRequest, controller signs a new cert
// for that public key + same SPIFFE ID.
//
// OR even simpler: add public_key_pem to RenewCertRequest (one extra field).
```

Wait — there is one thing to decide here. To issue a new cert, the controller
needs the connector's public key. It does not store the public key separately —
it only stores the cert. Two clean options:

**Option A — Connector sends its current public key in RenewCertRequest**

```protobuf
message RenewCertRequest {
  string connector_id    = 1;  // logging only
  bytes  public_key_der  = 2;  // connector's existing EC P-384 public key (DER)
}
```

On the Rust side, the connector reads its existing `connector.key` and extracts
the public key. Easy with `rcgen` / `rustls`.

**Option B — Controller reads public key from existing cert in DB**

The existing cert PEM is not stored in the DB (only `cert_serial` and
`cert_not_after` are stored). So this would require a DB schema change.

**Use Option A.** One extra field in the proto, zero DB changes.
The connector already has its key on disk — extracting the public key is trivial.

Update the proto:

```protobuf
message RenewCertRequest {
  string connector_id   = 1;  // logging only
  bytes  public_key_der = 2;  // DER-encoded EC P-384 public key from connector.key
}
```

Then `RenewConnectorCert` in `pki/workspace.go`:

```go
// RenewConnectorCert issues a fresh 7-day cert for an existing connector.
// The connector's keypair is unchanged — only the cert validity window is renewed.
// Same SPIFFE SAN, same CN, fresh NotBefore/NotAfter.
func (s *service) RenewConnectorCert(
    ctx         context.Context,
    tenantID    string,
    connectorID string,
    trustDomain string,
    publicKeyDER []byte,   // connector's existing public key
    certTTL     time.Duration,
) (*ConnectorCertResult, error) {

    // Parse the public key from DER bytes sent by the connector
    pubKey, err := x509.ParsePKIXPublicKey(publicKeyDER)
    if err != nil {
        return nil, fmt.Errorf("pki: parse public key: %w", err)
    }

    // Build SPIFFE URI — same as enrollment, same identity
    spiffeID := appmeta.ConnectorSPIFFEID(trustDomain, connectorID)
    uri, _   := url.Parse(spiffeID)

    now := time.Now().UTC()
    cert := &x509.Certificate{
        SerialNumber: newSerial(),
        Subject: pkix.Name{
            CommonName:   appmeta.PKIConnectorCNPrefix + connectorID,
            Organization: []string{appmeta.PKIWorkspaceOrganization},
        },
        URIs:      []*url.URL{uri},
        NotBefore: now,
        NotAfter:  now.Add(certTTL),
        KeyUsage:  x509.KeyUsageDigitalSignature,
        ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
        IsCA: false,
    }

    // Load + decrypt WorkspaceCA key (same pattern as SignConnectorCert)
    workspaceCA, caKey, err := s.loadWorkspaceCA(ctx, tenantID)
    if err != nil {
        return nil, fmt.Errorf("pki: load workspace CA: %w", err)
    }
    defer zeroKey(caKey)

    // Sign with the connector's EXISTING public key — no new keypair
    certDER, err := x509.CreateCertificate(rand.Reader, cert, workspaceCA, pubKey, caKey)
    if err != nil {
        return nil, fmt.Errorf("pki: sign renewal cert: %w", err)
    }

    return &ConnectorCertResult{
        CertificatePEM:   pemEncode("CERTIFICATE", certDER),
        WorkspaceCAPEM:   s.workspaceCACertPEM(ctx, tenantID),
        IntermediateCAPEM: s.intermediateCertPEM(),
        Serial:           cert.SerialNumber.Text(16),
        NotBefore:        cert.NotBefore,
        NotAfter:         cert.NotAfter,
    }, nil
}
```

---

## Step 5 — Rust Connector Changes (connector/src/heartbeat.rs + new renewal.rs)

### heartbeat.rs — react to re_enroll=true

Find the part where you currently handle `re_enroll`:

```rust
// Current code (logs warning, does nothing):
if resp.re_enroll {
    tracing::warn!("controller requested re-enrollment — not yet implemented");
}

// New code:
if resp.re_enroll {
    tracing::info!("cert renewal requested by controller — starting renewal");
    match renewal::renew_cert(&state, &cfg).await {
        Ok(new_state) => {
            // Update state in place — heartbeat loop continues with new cert
            // The tonic channel needs to be rebuilt with the new cert
            // Signal the heartbeat loop to reconnect
            tracing::info!(
                "cert renewed successfully, new expiry: {}",
                new_state.cert_not_after
            );
            // Return so the loop restarts with the fresh cert
            return Ok(new_state);
        }
        Err(e) => {
            // Don't panic — old cert still has ~48h left
            // Log and keep heartbeating, will retry on next heartbeat
            tracing::error!("cert renewal failed, will retry: {}", e);
        }
    }
}
```

### connector/src/renewal.rs — new file

This is the renewal flow. Much simpler than enrollment because:
- No JWT to parse
- No CA fingerprint to verify
- No new keypair to generate
- No config file to rewrite
- Just: read existing key → extract public key → call RenewCert → save new cert

```rust
// src/renewal.rs

use crate::{appmeta, config::ConnectorConfig, crypto, tls};
use crate::state::ConnectorState;

/// Called when heartbeat response contains re_enroll=true.
///
/// The connector keeps its existing EC P-384 keypair.
/// We just get a fresh cert for the same key + same SPIFFE identity.
///
/// Steps:
///   1. Read existing private key from disk
///   2. Extract public key in DER format
///   3. Call RenewCert RPC over existing mTLS connection
///   4. Save new connector.crt to disk
///   5. Update state.json with new cert_not_after
///   6. Return updated state so heartbeat loop rebuilds its mTLS channel
pub async fn renew_cert(
    state: &ConnectorState,
    cfg: &ConnectorConfig,
) -> anyhow::Result<ConnectorState> {

    // 1. Read existing private key from disk
    let key_pem = tokio::fs::read_to_string(&cfg.key_path()).await?;

    // 2. Extract public key in DER — the controller needs this to sign a new cert
    let public_key_der = crypto::extract_public_key_der(&key_pem)?;

    // 3. Build RenewCert RPC request
    let req = RenewCertRequest {
        connector_id:   state.connector_id.clone(),  // logging only on server
        public_key_der: public_key_der,
    };

    // 4. Call RenewCert over mTLS (uses existing cert — still valid for ~48h)
    let mut client = build_mtls_client(state, cfg).await?;
    let resp = client.renew_cert(req).await?.into_inner();

    // 5. Save new connector.crt
    tokio::fs::write(&cfg.cert_path(), &resp.certificate_pem).await?;

    // 6. Save updated CA chain (in case it changed)
    let ca_chain = format!(
        "{}\n{}",
        String::from_utf8_lossy(&resp.workspace_ca_pem),
        String::from_utf8_lossy(&resp.intermediate_ca_pem),
    );
    tokio::fs::write(&cfg.ca_path(), ca_chain.as_bytes()).await?;

    // 7. Parse new cert_not_after from the cert PEM
    let new_not_after = crypto::parse_cert_not_after(&resp.certificate_pem)?;

    // 8. Update state.json
    let new_state = ConnectorState {
        connector_id:   state.connector_id.clone(),
        trust_domain:   state.trust_domain.clone(),
        enrolled_at:    state.enrolled_at.clone(),
        cert_not_after: new_not_after.to_rfc3339(),
    };
    new_state.save(&cfg.state_path()).await?;

    tracing::info!(
        "cert renewed. connector_id={} new_expiry={}",
        state.connector_id,
        new_not_after
    );

    Ok(new_state)
}
```

### crypto.rs — two small helpers needed

You need to add two functions to `src/crypto.rs`:

```rust
/// Extract the public key from a PEM-encoded EC private key, return as DER bytes.
/// The controller needs this to sign a renewal cert for the same keypair.
pub fn extract_public_key_der(private_key_pem: &str) -> anyhow::Result<Vec<u8>> {
    // Parse the private key PEM → extract public key → encode as DER
    // Use rcgen or rustls-pemfile + p384 crate
}

/// Parse the NotAfter timestamp from a PEM certificate.
/// Used to update state.json after renewal.
pub fn parse_cert_not_after(cert_pem: &[u8]) -> anyhow::Result<chrono::DateTime<chrono::Utc>> {
    // Use x509-parser (already a dependency) to read NotAfter field
}
```

---

## Files You Touch — Complete List

```
controller/
  proto/connector.proto              MODIFY — add RenewCert RPC + messages + public_key_der field
  internal/connector/config.go       MODIFY — add RenewalWindow field
  internal/connector/heartbeat.go    MODIFY — check cert_not_after, set re_enroll=true
  internal/connector/renewal.go      NEW    — RenewCert handler
  internal/pki/workspace.go          MODIFY — add RenewConnectorCert method
  cmd/server/main.go                 MODIFY — add mustDuration for CONNECTOR_RENEWAL_WINDOW
  .env (+ .env.example)              MODIFY — add CONNECTOR_RENEWAL_WINDOW=48h

connector/
  proto/connector.proto              MODIFY — same proto (copy from controller)
  src/heartbeat.rs                   MODIFY — react to re_enroll=true
  src/renewal.rs                     NEW    — renew_cert() function
  src/crypto.rs                      MODIFY — add extract_public_key_der + parse_cert_not_after
  src/main.rs                        MODIFY — add renewal module declaration (mod renewal;)
```

**Nothing else changes.** No DB migration. No GraphQL changes. No frontend changes.
No admin action. No new env vars except `CONNECTOR_RENEWAL_WINDOW`.

---

## Order To Do It (Suggested)

Since you are doing this solo, do it in this order so you can test each piece:

```
1. Proto first
   → Add RenewCert RPC + messages to connector.proto
   → Run protoc on Go side → check stubs compile
   → cargo build on Rust side → check stubs compile
   → If both compile cleanly, continue

2. Controller config
   → Add RenewalWindow to ConnectorConfig
   → Add CONNECTOR_RENEWAL_WINDOW=48h to .env
   → Add mustDuration call in main.go
   → go build → confirm it compiles

3. PKI renewal method
   → Add RenewConnectorCert to pki/workspace.go
   → go build → confirm it compiles

4. RenewCert handler
   → Create internal/connector/renewal.go
   → Register the new RPC on the gRPC server in main.go
   → go build → confirm it compiles

5. Heartbeat re_enroll check
   → Modify heartbeat.go to set re_enroll=true when within renewal window
   → Test: set CONNECTOR_CERT_TTL=2m and CONNECTOR_RENEWAL_WINDOW=90s
     → enroll a connector → wait ~30s → heartbeat should return re_enroll=true
   → Check logs to confirm

6. Rust crypto helpers
   → Add extract_public_key_der to crypto.rs
   → Add parse_cert_not_after to crypto.rs
   → cargo build → confirm it compiles

7. Rust renewal.rs
   → Write renewal.rs
   → cargo build → confirm it compiles

8. Wire heartbeat.rs to call renewal
   → Modify heartbeat.rs to call renewal::renew_cert when re_enroll=true
   → cargo build → confirm it compiles

9. End-to-end test
   → Set short TTLs: CONNECTOR_CERT_TTL=3m, CONNECTOR_RENEWAL_WINDOW=2m
   → Enroll a connector
   → Wait 1 minute
   → Watch logs: controller should send re_enroll=true
   → Watch logs: connector should call RenewCert + save new cert
   → Check state.json: cert_not_after should be ~3 minutes from renewal time
   → Connector stays ACTIVE throughout — never goes DISCONNECTED
   → Reset TTLs back to production values (CONNECTOR_CERT_TTL=168h)
```

---

## How to Test Without Waiting 5 Days

Use short TTLs during development. Set these in your `.env` temporarily:

```env
CONNECTOR_CERT_TTL=3m          # cert expires in 3 minutes
CONNECTOR_RENEWAL_WINDOW=2m    # start renewing when < 2 minutes left
CONNECTOR_HEARTBEAT_INTERVAL=5s  # heartbeat every 5s so you see it fast
```

Timeline with these values:
```
0:00  Enroll connector    → 3-minute cert issued
1:00  Heartbeat           → 2 minutes left → re_enroll=true sent
1:00  Connector receives  → calls RenewCert
1:00  New cert issued     → 3 more minutes from now
3:00  Old cert expires    → doesn't matter, connector already on new cert
```

You can watch the whole cycle in under 2 minutes.
Reset to production values after testing.

---

## What Is NOT in This Plan (Out of Scope)

```
CRL/OCSP revocation        — still DB status flag only
Agent cert renewal         — same pattern, next sprint
Client cert renewal        — same pattern, future sprint
Renewal failure alerting   — connector just retries on next heartbeat
Admin notification         — no UI changes needed
```

---

## Summary

```
What you are building:   Automatic cert renewal before 7-day certs expire

New proto RPC:           RenewCert (mTLS, no JWT, connector sends public key)
New controller file:     internal/connector/renewal.go (RenewCert handler)
New Rust file:           src/renewal.rs (renew_cert function)
Modified files:          9 total (proto, config, heartbeat×2, pki, main, crypto, .env)

New env var:             CONNECTOR_RENEWAL_WINDOW=48h
No DB migration:         cert_not_after already in connectors table
No frontend changes:     renewal is invisible to the admin UI
No admin action ever:    fully automatic once deployed

Test with:               CONNECTOR_CERT_TTL=3m CONNECTOR_RENEWAL_WINDOW=2m
                         Watch the whole cycle in under 2 minutes

Done criteria:           Connector stays ACTIVE forever without manual re-enrollment
                         cert_not_after in state.json keeps extending automatically
```
