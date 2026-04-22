// appmeta.rs — SPIFFE identity and PKI constants for the ZECURITY Shield
//
// WHY THIS FILE EXISTS:
//   The Go controller, Rust connector, and Rust shield must all agree on
//   identity strings. If the controller signs a cert with:
//     "spiffe://ws-acme.zecurity.in/shield/abc-123"
//   but the shield expects:
//     "spiffe://ws-acme.zecurity.io/shield/abc-123"
//   then mTLS verification fails and the shield can never connect.
//
//   This file mirrors controller/internal/appmeta/identity.go CHARACTER-FOR-CHARACTER.
//   It is the single source of truth for identity strings on the Rust side.
//
// RULES:
//   1. No other .rs file writes "zecurity.in", "shield", "connector", or any
//      SPIFFE string as a literal. Always import from here.
//   2. If the Go appmeta/identity.go changes, update this file immediately.
//   3. The #[cfg(test)] block verifies internal consistency.
//
// USED BY:
//   enrollment.rs  — builds CSR CN ("shield-<id>") and SAN URI using shield_spiffe_id()
//   tls.rs         — verifies connector cert contains expected SPIFFE URI
//   control_stream.rs — builds expected connector SPIFFE ID for mTLS verification

// ── Product identity ─────────────────────────────────────────────────────────

/// Human-readable product name. Used in log messages.
/// Go equivalent: appmeta.ProductName
pub const PRODUCT_NAME: &str = "ZECURITY";

// ── SPIFFE identity constants ─────────────────────────────────────────────────

/// Global trust domain — the root of all ZECURITY SPIFFE identities.
/// The controller's own certificate uses this domain.
/// Go equivalent: appmeta.SPIFFEGlobalTrustDomain
pub const SPIFFE_GLOBAL_TRUST_DOMAIN: &str = "zecurity.in";

/// Full SPIFFE URI embedded in the controller's TLS certificate.
/// The shield verifies this during enrollment (plain TLS to controller).
/// Go equivalent: appmeta.SPIFFEControllerID
pub const SPIFFE_CONTROLLER_ID: &str = "spiffe://zecurity.in/controller/global";

/// Workspace trust domain prefix. Never concatenate manually — use workspace_trust_domain().
/// Go equivalent: appmeta.SPIFFETrustDomainPrefix
pub const SPIFFE_TRUST_DOMAIN_PREFIX: &str = "ws-";

/// Workspace trust domain suffix. Never concatenate manually — use workspace_trust_domain().
/// Go equivalent: appmeta.SPIFFETrustDomainSuffix
pub const SPIFFE_TRUST_DOMAIN_SUFFIX: &str = ".zecurity.in";

/// SPIFFE role segment for shields. Used in CSR SAN URI during enrollment.
/// Example URI: "spiffe://ws-acme.zecurity.in/shield/<shield_id>"
/// Go equivalent: appmeta.SPIFFERoleShield
pub const SPIFFE_ROLE_SHIELD: &str = "shield";

/// SPIFFE role segment for connectors. Used in tls.rs to verify the
/// connector's certificate during mTLS control-stream handshake.
/// Go equivalent: appmeta.SPIFFERoleConnector
pub const SPIFFE_ROLE_CONNECTOR: &str = "connector";

/// SPIFFE role segment for the controller.
/// Go equivalent: appmeta.SPIFFERoleController
pub const SPIFFE_ROLE_CONTROLLER: &str = "controller";

// ── PKI constants ─────────────────────────────────────────────────────────────

/// Certificate CN prefix for shields. CN = "shield-<shieldID>".
/// Used by enrollment.rs when building the CSR.
/// Go equivalent: appmeta.PKIShieldCNPrefix
pub const PKI_SHIELD_CN_PREFIX: &str = "shield-";

/// Certificate CN prefix for connectors.
/// Go equivalent: appmeta.PKIConnectorCNPrefix
pub const PKI_CONNECTOR_CN_PREFIX: &str = "connector-";

// ── Network constants ─────────────────────────────────────────────────────────

/// Name of the TUN interface created by network.rs during enrollment.
/// The shield creates this interface and assigns its interface_addr to it.
/// Go equivalent: appmeta.ShieldInterfaceName
pub const SHIELD_INTERFACE_NAME: &str = "zecurity0";

/// CIDR range from which interface addresses are assigned by the controller.
/// The controller picks a /32 from this range for each shield.
/// Go equivalent: appmeta.ShieldInterfaceCIDR
pub const SHIELD_INTERFACE_CIDR_RANGE: &str = "100.64.0.0/10";

// ── Helper functions ──────────────────────────────────────────────────────────

/// Derives the SPIFFE trust domain for a workspace.
///
/// Example: `workspace_trust_domain("acme")` → `"ws-acme.zecurity.in"`
///
/// Mirrors Go's `appmeta.WorkspaceTrustDomain(slug)`.
/// Used by enrollment.rs to build the CSR SAN URI.
pub fn workspace_trust_domain(slug: &str) -> String {
    format!(
        "{}{}{}",
        SPIFFE_TRUST_DOMAIN_PREFIX, slug, SPIFFE_TRUST_DOMAIN_SUFFIX
    )
}

/// Builds the full SPIFFE URI for a shield certificate.
///
/// Example: `shield_spiffe_id("ws-acme.zecurity.in", "abc-123")`
///        → `"spiffe://ws-acme.zecurity.in/shield/abc-123"`
///
/// Mirrors Go's `appmeta.ShieldSPIFFEID(trustDomain, shieldID)`.
/// Used by enrollment.rs to set the CSR SAN URI.
pub fn shield_spiffe_id(trust_domain: &str, shield_id: &str) -> String {
    format!(
        "spiffe://{}/{}/{}",
        trust_domain, SPIFFE_ROLE_SHIELD, shield_id
    )
}

/// Builds the full SPIFFE URI for a connector certificate.
///
/// Example: `connector_spiffe_id("ws-acme.zecurity.in", "xyz-456")`
///        → `"spiffe://ws-acme.zecurity.in/connector/xyz-456"`
///
/// Used by tls.rs to verify the connector's identity during mTLS control streams.
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
    fn controller_id_matches_composition() {
        // Ensures SPIFFE_CONTROLLER_ID is consistent with SPIFFE_GLOBAL_TRUST_DOMAIN.
        // If someone changes the trust domain, this test will catch the mismatch.
        let expected = format!("spiffe://{}/controller/global", SPIFFE_GLOBAL_TRUST_DOMAIN);
        assert_eq!(SPIFFE_CONTROLLER_ID, expected);
    }

    #[test]
    fn trust_domain_suffix_matches_global() {
        let expected = format!(".{}", SPIFFE_GLOBAL_TRUST_DOMAIN);
        assert_eq!(SPIFFE_TRUST_DOMAIN_SUFFIX, expected);
    }

    #[test]
    fn workspace_trust_domain_format() {
        assert_eq!(workspace_trust_domain("acme"), "ws-acme.zecurity.in");
    }

    #[test]
    fn shield_spiffe_id_format() {
        assert_eq!(
            shield_spiffe_id("ws-acme.zecurity.in", "abc-123"),
            "spiffe://ws-acme.zecurity.in/shield/abc-123"
        );
    }

    #[test]
    fn connector_spiffe_id_format() {
        assert_eq!(
            connector_spiffe_id("ws-acme.zecurity.in", "xyz-456"),
            "spiffe://ws-acme.zecurity.in/connector/xyz-456"
        );
    }
}
