# Phase 5 — Rust Connector: Enrollment Flow

Implement the one-time enrollment process that exchanges a JWT for a signed SPIFFE certificate.

---

## Files to Create

```
connector/src/enrollment.rs
connector/src/crypto.rs
```

---

## `connector/src/enrollment.rs` — Enrollment Flow

1. Parse JWT payload (base64-decode middle segment — **no signature verification**, connector has no `JWT_SECRET`; trust is established via CA fingerprint)
2. Extract `connector_id`, `workspace_id`, `trust_domain`, `ca_fingerprint`, `jti`
3. `GET http://<CONTROLLER_ADDR>/ca.crt` — fetch Intermediate CA cert
4. SHA-256 of fetched cert DER → compare hex against `ca_fingerprint` from JWT
   - **Mismatch → `exit(1)`** with clear MITM warning
5. Generate EC P-384 keypair, save private key to `connector.key` (mode 0600)
6. Build CSR:
   - CN: `format!("{}{}", appmeta::PKI_CONNECTOR_CN_PREFIX, connector_id)`
   - SAN URI: `format!("spiffe://{}/{}/{}", trust_domain, appmeta::SPIFFE_ROLE_CONNECTOR, connector_id)`
7. Connect to controller gRPC — plain TLS (not mTLS), trust root = fetched CA
8. Call `Enroll { enrollment_token, csr_der, version, hostname }`
9. Save: `connector.crt`, `workspace_ca.crt` (workspace + intermediate chain), `state.json`
10. Remove `ENROLLMENT_TOKEN` from config, write `CONNECTOR_ID`

---

## `connector/src/crypto.rs` — Crypto Utilities

- EC P-384 key generation
- PEM read/write
- CSR building via `rcgen`

---

## Important Rules

1. **The Rust connector never has `JWT_SECRET`.** It cannot verify the enrollment JWT signature. Trust is established by verifying the CA certificate fingerprint embedded in the JWT against the actual fetched certificate.
2. **Needs proto stubs from Phase 4** (`build.rs` must compile successfully).
3. **Needs `appmeta.rs` from Phase 4** (SPIFFE constants must be committed).

---

## Phase 5 Checklist

```
✓ enrollment.rs implements full enrollment flow
✓ JWT payload parsed without signature verification
✓ CA fingerprint verified before proceeding
✓ EC P-384 keypair generated and saved securely
✓ CSR built with correct CN and SPIFFE SAN URI
✓ gRPC Enroll call succeeds with plain TLS
✓ Certificate chain saved correctly
✓ ENROLLMENT_TOKEN removed from config after enrollment
✓ crypto.rs provides key/CSR utilities
✓ Committed and pushed
```

---

## After This Phase

Then proceed to Phase 6 (heartbeat + TLS).
