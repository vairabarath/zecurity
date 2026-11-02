# PENDING-20: Geofencing for resource access

**Status:** approved 2026-11-02. **G1a (backend) built** on local branch `feat/geofencing-g1` (not pushed); gate green. Next: G1b console.
**Date:** 2026-11-02
**Base:** `fixed-pendings`. Builds on the device-posture pipeline (ADR-030, migrations 030 and 044). It assumes PENDING-19 (workspace roles, PR #168) is merged: profile writes need `POSTURE_WRITE`.
**Scope:** its own epic.

---

## 1. Problem

Admins want to allow or block access to a resource by **where the device is**. Examples: "only from India", or "never from these countries". Today nothing in Zecurity knows a device's location:

- no GeoIP data or library;
- no stored client IP (`client_devices` has no IP column);
- the client has no OS location code, and it is Linux-only.

## 2. What exists that we build on (facts, 2026-11-02)

- **Device posture.**
  - The client reports checks every 300 s (`ReportDevicePosture`).
  - The controller validates and stores each report and evaluates every device profile (`internal/posture/evaluate.go`).
  - A resource bound to an **enforce** profile admits a device only while some bound profile is satisfied. `policy/compiler.go` `applyPosture` keeps the device's SPIFFE ID in `ACLEntry.allowed_spiffe_ids`.
  - Connectors enforce on SPIFFE ID alone and cut live sessions that lose access.
- **Server-attested checks** exist: `zecurity.device.approved` is computed by the controller, not reported by the client (ADR-030).
- **The client's public IP is visible only at the controller.**
  - The controller terminates TLS for client gRPC on :9090 and is not behind a proxy, so the gRPC peer address is the client's public (or NAT egress) address.
  - Connectors do not see it once traffic is relayed: the relay protocol does not carry it.
- **Freshness:**
  - reports every 5 min;
  - a satisfied evaluation is valid for 10 min after `received_at`;
  - policy changes reach connectors in seconds.

## 3. Decisions taken (2026-11-02, Sathiya)

| # | Decision |
|---|---|
| G-1 | **Location source: both, in two phases.** Phase 1: the public IP the controller observes, looked up in a GeoIP database (country level). Phase 2 (later, own design): the device's OS location. |
| G-2 | **Rule shape: allow and deny lists of countries.** Deny wins over allow. |
| G-3 | **GeoIP data: DB-IP Lite Country** (CC BY 4.0, no account or key, monthly, `.mmdb` format). It is loaded from a local file, so no client IP leaves the controller. The `.mmdb` format makes MaxMind a config change later. |
| G-4 | **Unknown location means deny** (fail closed), like every other posture check. Unknown covers: no recent observation, private or reserved address, no match in the database, lookup error, database not loaded. |
| G-5 | **Observe on posture reports and on the 60 s ACL poll** (was O-1). A country change is noticed within about 1 min; there is a DB write only when the country changes. |
| G-6 | **Store the country code plus the IP's /24 (IPv4) or /48 (IPv6) prefix**, never the full address (was O-2). |
| G-7 | **A dev-only override for lab and testing** (was O-3): `GEOIP_DEV_OVERRIDES="10.0.0.0/8=IN,127.0.0.0/8=US"`, ignored unless `ENV=development`. Unit tests use a generated `.mmdb`. |
| G-8 | **Deny-only rules are allowed** (was O-4): empty `allow` with a non-empty `deny`. Unknown is still denied. |
| G-9 | **No user-facing "why denied" in G1** (was O-5). The resource disappears for the device; the admin sees the device's observed country. |

## 4. Phase G1: country geofence (server-attested)

### 4.1 Model
- A new **server-computed posture check, `zecurity.network.country`**, like `zecurity.device.approved`.
  - The client never reports it.
  - The controller rejects it in a client report, as it already does for server-computed checks.
- **It is a requirement on a device profile**, with settings:
  - `allow`: an ISO 3166-1 alpha-2 list; empty means "any country not denied";
  - `deny`: an ISO list.
- **Evaluation:**
  1. `PASS` if the observed country is known, is not in `deny`, and (`allow` is empty or contains it).
  2. Otherwise `FAIL`.
  3. Unknown is `FAIL` (G-4). `allow_unsupported` does not apply to this check.
  4. A requirement with both lists empty is refused on save, so it can't silently allow everything.
- **Binding and modes are unchanged:** the existing `bindResourceToProfile`, enforce and audit modes, and OR across profiles. A resource that should be "approved company device **and** in India" puts both checks in one profile.

### 4.2 Observation
- **Where the IP is read:** in the controller's client gRPC handlers, from `peer.FromContext(ctx).Addr`, with IPv4-mapped IPv6 unmapped.
- **When:**
  - on `ReportDevicePosture` (every 5 min);
  - on the client's ACL poll (`GetACLSnapshot`, every 60 s), so a country change is seen within about a minute. See O-1.
- **What is stored:** per device, `observed_country` (ISO code or `''` for unknown), `observed_at`, and the database build date. Whether the IP itself is stored is O-2.
- **Re-evaluation:**
  - when the observed country changes, re-evaluate that device's profiles (existing per-device path), which notifies the ACL on change;
  - when the GeoIP database is reloaded, re-evaluate the workspace.
- **Freshness:** an observation older than 10 min counts as unknown, the same window as posture reports.

### 4.3 GeoIP database
- **Config:** `GEOIP_DB_PATH` points to a DB-IP Lite Country `.mmdb`. The library is `oschwald/maxminddb-golang` (pure Go, reads `.mmdb`).
- **Updates:**
  - an operator downloads the monthly file;
  - the controller reloads it when the file's mtime changes (checked hourly) and on start;
  - it logs and exports a metric for the database build date, and warns when the database is more than 60 days old.
- **Missing or broken database:** every lookup is unknown, so geofenced resources deny (G-4). The controller still starts and logs loudly.
- **Attribution:** CC BY 4.0 requires crediting DB-IP. Add "IP geolocation by DB-IP" to the console page that edits the check.

### 4.4 Console (admin)
- **Device profile editor:** a "Country" check with two country pickers (Allow, Deny) and a note:
  - location comes from the device's public IP;
  - a VPN shows the VPN's country;
  - unknown means denied.
- **Devices page:** shows each device's last observed country and time (read: `CONSOLE_READ`; editing profiles: `POSTURE_WRITE`).
- **Audit:** an audit row on profile changes, as today. No row per evaluation (too noisy). A country change on a device is logged server-side.

### 4.5 Schema (one migration, next free number at merge: `048_` if PR #168 is merged first)
- **Requirement settings:** `device_profile_requirements` gets `params JSONB NOT NULL DEFAULT '{}'`, validated per check. Only `zecurity.network.country` uses it now.
- **Observations:** a per-device row (`client_devices` columns or a small `device_network_observations` table), plus the IP if O-2 says so.
- PR label `[schema: reset DB]`.

### 4.6 What does not change
- Proto, connector, relay and shield.
- The enforcement path stays SPIFFE ID in `allowed_spiffe_ids`.

### 4.7 Known limits (to state in the console and docs)
- **VPNs, corporate proxies and mobile carriers** appear in their exit country.
- **Two paths:** the country is the one the controller sees. A device whose data path leaves through a different network (split tunnelling, multi-homing) is judged by its controller path.
- **Delay:** a move takes effect within about 1 min (ACL poll) to 5 min (report), and access is cut once the evaluation turns `FAIL`.
- **Precision:** country level only. DB-IP Lite has no VPN, hosting or Tor flags.

## 5. Phase G2: device OS location (later, own design)

Only outlined here. G2 needs its own note before any build.
- **Sources:** Windows Location API, macOS CoreLocation, Linux GeoClue. The client is Linux-only today, so G2 mostly waits for other client platforms.
- **Self-reported:** a compromised device can fake it. It should **never be used alone**; it should add to G1, for example "GeoIP country AND device location within X km".
- **Permissions and privacy:** OS location permission, and storing precise location (personal data). The likely design reduces it on the device to "inside or outside the configured area".
- **Rule shape:** radius or polygon areas. This is different from the G1 country lists.

## 6. Decisions still open

None. O-1 … O-5 were decided on 2026-11-02 as G-5 … G-9 (§3).

## 7. Tests (build gate)

- **Unit:**
  - the country rule (allow, deny, deny-wins, both empty refused, unknown is `FAIL`);
  - IP unmapping;
  - lookups against a generated test `.mmdb`.
- **DB-backed** (must run, not skip):
  - observation upsert and re-evaluation on country change;
  - stale observation is unknown;
  - a database reload re-evaluates;
  - `applyPosture` drops the device from `allowed_spiffe_ids` when the country check fails in an enforce profile and keeps it in audit mode;
  - the client cannot report `zecurity.network.country`.
- **Console:** the country editor; the parity test for the new capability usage (`POSTURE_WRITE`).
- **Live (lab):** with the dev override, a device mapped to an allowed country reaches a bound resource. Switching the override to a denied country cuts the session within the expected window.

## 8. Phases

1. **G1a, backend:** migration, observation, GeoIP loader and reload, check evaluation, validation, tests. **Built 2026-11-02:**
   - migration `048_geofencing.sql`;
   - `internal/geoip` (DB-IP Lite `.mmdb` read via `oschwald/maxminddb-golang/v2` v2.0.0; test databases via `maxmind/mmdbwriter` v1.1.0, versions chosen so no shared dependency moved);
   - `posture/country.go` (rule, observation store, evaluation);
   - `client/location.go` (peer-address observation on posture reports, and on ACL polls on change or every 2 min);
   - `main.go` (`GEOIP_DB_PATH`, `GEOIP_DEV_OVERRIDES` honoured only with `ENV=development`, hourly reload that re-evaluates workspaces, warning at 60 days);
   - GraphQL (`addProfileRequirement(countries)`, `DeviceProfileRequirement.countries`, `ClientDevice.observedCountry/observedNetwork/observedAt`).
2. **G1b, console:** profile editor with country pickers, the Devices page column, attribution.
3. **G1c, live acceptance** on the lab (O-3 override).
4. **G2:** a separate design note for device OS location.
