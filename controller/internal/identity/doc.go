package identity

// Identity-pipeline invariants (PENDING-04 / ADR-023, ADR-024). These are
// intentional boundaries — future contributors must not "simplify" them away.
//
//  1. IDENTITY KEY IS (connection, subject), NEVER EMAIL. Resolution and linking
//     key on external_identities(tenant_id, connection_id, subject). Email is at
//     most an invite-matching hint. Two IdPs, or two subjects on one IdP, are two
//     distinct canonical users even with the same email address.
//
//  2. NO SILENT MERGE. A first-seen (connection, subject) always yields a NEW
//     canonical user (or an invited join) — never an automatic merge into an
//     existing account by email. Admin-approved linking is a documented,
//     unshipped follow-up (ADR-024 options B/C).
//
//  3. LIFECYCLE LIVES ON THE CANONICAL USER. The gate (CheckLifecycle) reads
//     users.status, independent of any external identity or IdP. Only 'active'
//     proceeds; the pipeline fails CLOSED on anything else and never leaks which
//     state blocked login.
//
//  4. IDENTITY AND ITS LINK COMMIT ATOMICALLY. A Provisioner writes the users row
//     and its external_identities row in ONE transaction — an authenticated login
//     never leaves a user without an identity key, or a link without a user.
//
//  5. REVOCATION IS ENFORCED AT REFRESH, NOT PER REQUEST. Every access token
//     carries the identity_generation it was minted under. A bump (Revoker)
//     invalidates the refresh chain at the next /auth/refresh; the live access
//     token rides out its short TTL. This keeps the per-request path free of a DB
//     read while giving immediate practical revocation. See revocation.go.
//
//  6. GROUPS FROM THE TOKEN ARE HINTS, NOT AUTHORIZATION. AuthenticationContext
//     may carry GroupsHint, but the pipeline never persists it into anything the
//     policy engine reads. Effective membership stays in the internal groups
//     tables (kept fresh by PENDING-05 SCIM).
//
//  7. AUDIT IS NOT ON THE AVAILABILITY PATH. Identity events are published to the
//     audit sink best-effort: a failed audit_logs write is logged, but never
//     fails an otherwise-valid login or a completed admin action.
