// appmeta.rs — SPIFFE identity constants for the ZECURITY connector
//
// This file mirrors controller/internal/appmeta/identity.go CHARACTER-FOR-CHARACTER.
// Every string value here must match the Go source exactly.
//
// WHY THIS FILE EXISTS:
//   The Go controller and Rust connector must agree on identity strings.
//   If the controller signs a cert with "spiffe://zecurity.in/connector/abc-123"
//   but the Rust side expects "spiffe://zecurity.io/connector/abc-123", mTLS fails.
//   This file is the single source of truth on the Rust side.
//
// RULES:
//   1. No other .rs file writes "zecurity.in", "ws-", "connector", or any SPIFFE
//      string as a literal. Always import from here.
//   2. If Member 3 changes a constant in Go's identity.go, update this file immediately.
//   3. The #[cfg(test)] block verifies internal consistency (e.g., SPIFFE_CONTROLLER_ID
//      must equal "spiffe://" + SPIFFE_GLOBAL_TRUST_DOMAIN + "/controller/global").
//
// USED BY:
//   enrollment.rs (Phase 5) — builds CSR CN and SAN URI using PKI_CONNECTOR_CN_PREFIX,
//                              SPIFFE_ROLE_CONNECTOR, workspace_trust_domain(), connector_spiffe_id()
//   tls.rs (Phase 6)        — verifies controller cert contains SPIFFE_CONTROLLER_ID
//   heartbeat.rs (Phase 6)  — uses connector_spiffe_id() for identity context

// ── Product identity ────────────────────────────────────────────────────────

/// Product name — used in log messages. Matches Go's appmeta.ProductName.
pub const PRODUCT_NAME: &str = "ZECURITY";

// ── SPIFFE identity constants ───────────────────────────────────────────────

/// Global trust domain. The root of all ZECURITY SPIFFE identities.
/// Go equivalent: appmeta.SPIFFEGlobalTrustDomain
pub const SPIFFE_GLOBAL_TRUST_DOMAIN: &str = "zecurity.in";

/// Full SPIFFE URI of the controller. The Rust connector verifies this
/// in the controller's TLS cert on every mTLS heartbeat handshake (Phase 6 tls.rs).
/// Go equivalent: appmeta.SPIFFEControllerID
pub const SPIFFE_CONTROLLER_ID: &str = "spiffe://zecurity.in/controller/global";

/// Workspace trust domain prefix. Do not concatenate manually — use workspace_trust_domain().
/// Go equivalent: appmeta.SPIFFETrustDomainPrefix
pub const SPIFFE_TRUST_DOMAIN_PREFIX: &str = "ws-";

/// Workspace trust domain suffix. Do not concatenate manually — use workspace_trust_domain().
/// Go equivalent: appmeta.SPIFFETrustDomainSuffix
pub const SPIFFE_TRUST_DOMAIN_SUFFIX: &str = ".zecurity.in";

/// SPIFFE role segment for connectors. Used in CSR SAN URI during enrollment.
/// Go equivalent: appmeta.SPIFFERoleConnector
pub const SPIFFE_ROLE_CONNECTOR: &str = "connector";

/// SPIFFE role segment for agents (future sprint — plumbed now for forward compatibility).
/// Go equivalent: appmeta.SPIFFERoleAgent
pub const SPIFFE_ROLE_AGENT: &str = "agent";

/// SPIFFE role segment for the controller.
/// Go equivalent: appmeta.SPIFFERoleController
pub const SPIFFE_ROLE_CONTROLLER: &str = "controller";

// ── PKI constants ───────────────────────────────────────────────────────────

/// Certificate CN prefix for connectors. CN = "connector-<connectorID>".
/// Used by enrollment.rs (Phase 5) when building the CSR.
/// Go equivalent: appmeta.PKIConnectorCNPrefix
pub const PKI_CONNECTOR_CN_PREFIX: &str = "connector-";

/// Certificate CN prefix for agents (future sprint).
/// Go equivalent: appmeta.PKIAgentCNPrefix
pub const PKI_AGENT_CN_PREFIX: &str = "agent-";

// ── Helper functions ────────────────────────────────────────────────────────

/// Derives the SPIFFE trust domain for a workspace.
///
/// Example: `workspace_trust_domain("acme")` → `"ws-acme.zecurity.in"`
///
/// Mirrors Go's `appmeta.WorkspaceTrustDomain(slug)`.
/// Used by enrollment.rs (Phase 5) to build the CSR SAN URI.
pub fn workspace_trust_domain(slug: &str) -> String {
    format!(
        "{}{}{}",
        SPIFFE_TRUST_DOMAIN_PREFIX, slug, SPIFFE_TRUST_DOMAIN_SUFFIX
    )
}

/// Builds the full SPIFFE URI for a connector certificate.
///
/// Example: `connector_spiffe_id("ws-acme.zecurity.in", "abc-123")`
///        → `"spiffe://ws-acme.zecurity.in/connector/abc-123"`
///
/// Mirrors Go's `appmeta.ConnectorSPIFFEID(trustDomain, connectorID)`.
/// Used by enrollment.rs (Phase 5) to set the CSR SAN URI,
/// and by the controller's enrollment handler to verify the CSR.
pub fn connector_spiffe_id(trust_domain: &str, connector_id: &str) -> String {
    format!(
        "spiffe://{}/{}/{}",
        trust_domain, SPIFFE_ROLE_CONNECTOR, connector_id
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_workspace_trust_domain() {
        assert_eq!(workspace_trust_domain("acme"), "ws-acme.zecurity.in");
        assert_eq!(workspace_trust_domain("test"), "ws-test.zecurity.in");
        assert_eq!(
            workspace_trust_domain("prod-workspace"),
            "ws-prod-workspace.zecurity.in"
        );
    }

    #[test]
    fn test_connector_spiffe_id() {
        assert_eq!(
            connector_spiffe_id("ws-acme.zecurity.in", "abc-123"),
            "spiffe://ws-acme.zecurity.in/connector/abc-123"
        );
    }

    #[test]
    fn test_controller_id_matches_composition() {
        let expected = format!("spiffe://{}/controller/global", SPIFFE_GLOBAL_TRUST_DOMAIN);
        assert_eq!(SPIFFE_CONTROLLER_ID, expected);
    }

    #[test]
    fn test_trust_domain_suffix_matches_global() {
        let expected = format!(".{}", SPIFFE_GLOBAL_TRUST_DOMAIN);
        assert_eq!(SPIFFE_TRUST_DOMAIN_SUFFIX, expected);
    }
}
