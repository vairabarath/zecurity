# D21 (O13) review note — a hostile revoked connector at the shield

**To:** Sathiya · **From:** Barath · **Date:** 2026-10-26 · **Status:** for discussion. No code changed.

## Where we are

D21 (2026-10-22, = O13) approved shield pruning (Phase E, done) and **accepted** this limit: a *hostile* revoked connector "can't be enforced at the shield with current interfaces" (feasibility report §19.8). The limit is "bounded by certificate lifetime", and clients (C) and relays (D) refuse it (design §10).

That is right for an *honest* revoked connector: it closes its shield streams (B2) and the shield fails over (Phase F fix). This note is only about the hostile case: a lost, stolen or compromised connector that keeps running with a still-valid certificate.

## Three facts the D21 decision didn't have

1. **The shield is a pivot, not just a path to protected resources.**
   - `shield/src/tunnel.rs` `handle_tunnel_open_tcp` connects to whatever `destination:port` the connector sends. There is no allowlist, not even "only the resources I protect".
   - Resource instructions from the connector also drive the shield's nftables rules.
   - So the hostile connector can reach any host the shield's machine can reach, and loosen its protection.
   - Client and relay refusal doesn't help here: this path never goes through a client or a relay.
2. **The window is up to 7 days.** `CONNECTOR_CERT_TTL` defaults to 7 days (`cmd/server/main.go`). Bug 7 stops a revoked connector from renewing, so the window can't be extended, but nothing shortens it.
3. **The missing interface now exists.**
   - **Complete:** since bug 9 (migration 039), the workspace CRL (`/ca.crl`) lists every unexpired certificate of a revoked connector, including after renewal and after the connector row is deleted. That also closes feasibility §19.9.
   - **Verifiable:** since bug 6, CRL consumers check it against exactly the Workspace CA (`crl_issuers`), so a swapped CRL is rejected.
   - The shield already holds that Workspace CA, the one it verifies its connector with.

## Options

| | What | Stops the hostile connector? | Cost |
|---|---|---|---|
| **A** | Shield checks the workspace CRL: fetch `/ca.crl` like the connector does, refuse a listed connector serial at connect, and drop the stream when a refresh lists it | **Yes**, within the refresh interval (~60 s) | Shield Rust change (CRL manager + `crl_issuers`, a third copy); the shield needs controller HTTP at runtime (today only at enrollment); a fail-closed vs last-good policy is needed |
| **C** | Shorter connector certificates (e.g. 24 h) | Shrinks the window, doesn't close it | Config only; more renewals (Phase M numbers would tell us if that's fine) |
| **D** | Shield tunnel allowlist: open tunnels only to the resources the shield protects | Limits the damage from *any* bad connector, revoked or not | Shield change; separate hardening. I'd like to do this one separately |

## Questions for you

1. **Keep D21 as accepted, or reopen it narrowly to add A?** My recommendation: reopen for A. A pivot into the shield's network for up to 7 days is more than "bounded by certificate lifetime" suggests.
2. **If A, what happens when the shield can't fetch the CRL?**
   - Fail closed straight away. Safest, but a controller outage would cut every shield off from its connector.
   - Keep the last good CRL until its `nextUpdate` (24 h), then fail closed. This matches the connector and client.

   I'd go with the second.
3. **Is a shorter `CONNECTOR_CERT_TTL` (C) acceptable as a stopgap,** given the Phase M measurements?
4. **Any objection to D as a separate task?** Does anything (discovery, Phase F, failover) rely on the shield tunnelling to destinations outside its protected resources?

Nothing will be built until we agree. A would change the F34 design (D21), so it's your call to reopen it.
