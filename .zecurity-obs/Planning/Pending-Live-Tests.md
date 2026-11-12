# Pending live tests (provider dashboard F series)

Merged features whose tracker box stays unticked until a live check on the lab passes. Run them in one session once the Must-have work is done, then tick each box in `Provider-Dashboard-Features.md`.

**Before the session**
- `main` pulled; local DB recreated (`cd controller && docker compose down -v && docker compose up -d`); migration `051_` is the latest one. This wipes local data, so bootstrap the super-admin again and enrol TOTP.
- Controller and provider console running; a super-admin signed in (TOTP); a relay-ops operator for the role checks.
- At least one tenant with resources and policies, and at least one relay enrolled (lab: `inkyank` 192.168.1.75, `manojinkyank` 192.168.1.53).
- Lab cautions: don't touch inkyank's `zecurity-relay` service (restart loop, missing config). Don't re-enable manoj's connector until `/etc/zecurity/ca.crt` is restored.

## Checklist

| ID | Feature | PR | Status |
|---|---|---|---|
| F8 | Tenant detail: policy counts | #190 | ☐ |
| F15 / F16 | CA hierarchy and CRL status (F15 OCSP closed, D-35) | #190 | ☐ |
| F25 | Control-plane health on Overview | #190 | ☐ |
| F38 | Audit export (CSV, JSON) | #190 | ☐ |
| F17 | Append-only audit log, before/after | #192 | ☐ |
| F5 | Relay bandwidth | #193 | ☐ |
| F14 | Daily certificate expiry email | PR pending | ☐ |

Add each Must-have item here as it merges.

## F8: Tenant detail policy counts
1. Open a tenant in the console (Tenants, then a tenant).
2. Compare the policy counts with the database (read-only query on the dev DB).
3. **Pass:** every count matches. An empty tenant shows 1 resource policy (the automatic default policy). Opening the detail writes one `tenant.read` audit row.

## F15 / F16: CA hierarchy (Certificates page)
1. Open Certificates and look at the CA hierarchy section.
2. **Pass:**
   - The tree reads Root, then Intermediate, then one workspace CA per tenant.
   - The intermediate shows the relay CRL (`/relay.crl`, covers relays); each workspace CA shows `/ca.crl?workspace_id=…` (covers client devices and connectors).
   - Revoke a test device, Refresh, and that workspace's CRL entry count goes up by one.
   - With more than 50 workspaces, "Show more" appends the rest and the count reads "Showing N of total".
   - No PEM bodies or key material appear anywhere on the page.
3. Sign in as relay-ops: the section says only super-admins can see it.

## F25: Control-plane health (Overview)
1. Open Overview and look at the Control plane card.
2. **Pass:**
   - Every component shows Healthy, with its latency.
   - Stop Valkey: after a Refresh it shows Unhealthy or Degraded with a fixed message (no raw error text). Start Valkey again and it returns to Healthy.
   - As relay-ops the card isn't available (`health.read` is super-admin only).

## F38: Audit export
1. Audit page: apply a filter (for example action = `relay.drain`), then export CSV and JSON.
2. **Pass:**
   - Both files hold exactly the rows the filter shows, newest first, with no duplicates.
   - CSV opens in a spreadsheet; a value starting with `=`, `+`, `-` or `@` shows as text (prefixed with `'`), not as a formula.
   - The JSON parses and matches the page.
   - Above 10,000 matching rows, the export stops at 10,000 and says it was truncated.

## F17: Append-only audit log, before/after
1. Perform state-changing actions in the console: change an operator's role, disable and enable an operator, reset an operator's TOTP, drain and resume a relay.
2. **Pass (details):** each audit row's details show `before` and `after`:
   - role change: role → role;
   - disable and enable: status `active` ↔ `disabled`;
   - TOTP reset: totp `enabled` → `none`;
   - drain and resume: draining false ↔ true.

   No passwords, hashes or TOTP secrets appear in the details.
3. **Pass (refusals):** against a **throwaway copy or the freshly recreated dev DB**, as the app user:
   - `UPDATE provider_audit_logs SET details = '{}' WHERE id = '<one id>'` fails with `provider_audit_logs is append-only: UPDATE refused` (SQLSTATE 42501);
   - `DELETE` and `TRUNCATE` fail the same way;
   - the row is unchanged afterwards.

## F5: Relay bandwidth
Needs the new relay build on a lab relay (heartbeat fields 9/10) and migration `052_`. Tip: set `RELAY_HEARTBEAT_DB_WRITE_INTERVAL=1m` on the controller to shorten the averaging window during the test.
1. Right after the relay starts, Relays page: Bandwidth shows **Measuring…** (one sample stored). After the next stored heartbeat it shows rates.
2. Push known traffic through the relay: for example `curl` a large file from a resource behind a connector, through a client that is placed on that relay. Note the bytes and the duration.
3. **Pass:**
   - "to clients" roughly matches bytes ÷ window (the tooltip and detail card show the window and "as of" time);
   - "to connectors" shows the much smaller request side;
   - an idle relay shows 0 B/s, not "Not reported".
4. Restart the relay service: the next stored sample shows **Measuring…** (no spike, no negative rate), then rates return.
5. An older relay build (without the fields) shows **Not reported**, and a revoked relay shows Not reported.
6. Relay journal: no errors; heartbeat unchanged otherwise (connection count, revocation answers still work).

## F14: Daily certificate expiry email
Needs SMTP configured on the controller (`SMTP_HOST`, `SMTP_PORT`, `SMTP_FROM` and the SMTP password) and a super-admin with a mailbox you can read.
1. Put something in scope: a relay whose certificate is expired or expires within 24 hours (for example a powered-off lab relay whose certificate lapsed), or a workspace CA within 30 days of expiry.
2. Restart the controller. About one minute later the email arrives (subject "Zecurity: N certificate(s) need attention").
3. **Pass:**
   - every active super-admin receives exactly one email; disabled super-admins and relay-ops receive none;
   - the email lists the expected certificates in the right section (Expired / within 24 hours / 7 days / 30 days) with name, serial and expiry time, and no tenant agent certificates;
   - restarting the controller again the same day sends nothing more;
   - the Audit page shows one `cert_expiry.alert_sent` row by `system:cert-expiry-alert` with the counts (no email addresses);
   - with nothing in scope, no email is sent.
4. With `CERT_EXPIRY_ALERTS=false`, the controller log says the alert is disabled and nothing is sent.

## Results log
| Date | ID | Result | Notes |
|---|---|---|---|
