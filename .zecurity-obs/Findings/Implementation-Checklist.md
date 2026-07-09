# Relay Findings Implementation Checklist

Source review: `.zecurity-obs/Relay-E2E-Flow-and-Security-Review.md`

## Fully Implemented

- [x] F5 — Probe rate-limit key bound to authenticated connector identity.
- [x] F6 — Client relay fallback and tunnel handshake are timeout-bounded.
- [x] F7 — Client net stack queues and buffers are bounded.
- [x] F9 — `max_connections == 0` relays are ineligible/low.
- [x] F11 — LabelledRelayList version is deterministic and content-addressed.
- [x] F15 — Debug print and `connetor_addr` typo are removed.
- [x] F4 — Relay expiry can mark relays inactive without violating DB constraints.

## Partially Implemented

- [~] F3 — Client rebuilds transports on action-triggered ACL changes.
  - Implemented: `sync`, `resources` TTL refresh, login/PostLoginState tunnel restart.
  - Remaining: standalone background ACL refresh loop, if required.

## Not Implemented

- [ ] F1 — Authenticate relay provisioning and burn single-use token/JTI.
- [ ] F2 — Add CRL/revocation checks for relay outer mTLS and controller relay heartbeat verification.
- [ ] F8 — Use controller/admin-stored SAN allowlists instead of request-provided SAN allowlists.
- [ ] F10 — Add relay certificate renewal.
- [ ] F12 — Narrow direct connector trust store to connector-specific roots only.
- [ ] F13 — Add Provision rate limiting/quota after F1.
- [ ] F14 — Make production relay address publishing operator-controlled/documented.
