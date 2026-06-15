use url::Url;
use uuid::Uuid;
use x509_parser::extensions::GeneralName;
use x509_parser::prelude::*;

const CONNECTOR_ROLE: &str = "connector";
const CLIENT_ROLE: &str = "client";
const RELAY_ROLE: &str = "relay";

#[derive(Debug, Clone)]
pub struct RelayIdentity {
    pub spiffe_id: String,
    pub trust_domain: String,
    pub relay_id: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ParsedSpiffe {
    pub uri: String,
    pub trust_domain: String,
    pub role: String,
    pub entity_id: String,
}

/// Extract exactly one SPIFFE URI SAN from a DER-encoded certificate.
pub fn extract_spiffe_uri(cert_der: &[u8]) -> Option<String> {
    let (_, cert) = X509Certificate::from_der(cert_der).ok()?;
    let san = cert.subject_alternative_name().ok()??;

    let mut spiffe_uris = san
        .value
        .general_names
        .iter()
        .filter_map(|name| match name {
            GeneralName::URI(uri) if uri.starts_with("spiffe://") => Some((*uri).to_string()),
            _ => None,
        });

    let uri = spiffe_uris.next()?;

    // Reject certificates containing multiple SPIFFE URI SANs.
    if spiffe_uris.next().is_some() {
        return None;
    }

    Some(uri)
}

/// Parse an exact `spiffe://<trust-domain>/<role>/<uuid>` identity.
pub fn parse_spiffe(spiffe_uri: &str) -> Option<ParsedSpiffe> {
    let url = Url::parse(spiffe_uri).ok()?;

    if url.scheme() != "spiffe"
        || url.username() != ""
        || url.password().is_some()
        || url.port().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
    {
        return None;
    }

    let trust_domain = url.host_str()?.to_string();
    if trust_domain.is_empty() {
        return None;
    }

    let segments: Vec<_> = url.path().trim_start_matches('/').split('/').collect();
    if segments.len() != 2 || segments.iter().any(|segment| segment.is_empty()) {
        return None;
    }

    let role = segments[0];
    let entity_id = segments[1];

    // Require canonical UUID representation.
    let uuid = Uuid::parse_str(entity_id).ok()?;
    if uuid.hyphenated().to_string() != entity_id.to_ascii_lowercase() {
        return None;
    }

    Some(ParsedSpiffe {
        uri: spiffe_uri.to_string(),
        trust_domain,
        role: role.to_string(),
        entity_id: entity_id.to_string(),
    })
}

pub fn validate_connector_spiffe(spiffe_uri: &str) -> bool {
    parse_spiffe(spiffe_uri)
        .map(|identity| identity.role == CONNECTOR_ROLE)
        .unwrap_or(false)
}

pub fn validate_client_spiffe(spiffe_uri: &str) -> bool {
    parse_spiffe(spiffe_uri)
        .map(|identity| identity.role == CLIENT_ROLE)
        .unwrap_or(false)
}

pub fn validate_relay_spiffe(spiffe_uri: &str) -> Option<RelayIdentity> {
    let identity = parse_spiffe(spiffe_uri)?;

    if identity.role != RELAY_ROLE {
        return None;
    }

    if identity.trust_domain != crate::appmeta::SPIFFE_GLOBAL_TRUST_DOMAIN {
        return None;
    }

    Some(RelayIdentity {
        spiffe_id: identity.uri,
        trust_domain: identity.trust_domain,
        relay_id: identity.entity_id,
    })
}

pub fn same_workspace(connector_spiffe: &str, client_spiffe: &str) -> bool {
    let Some(connector) = parse_spiffe(connector_spiffe) else {
        return false;
    };

    let Some(client) = parse_spiffe(client_spiffe) else {
        return false;
    };

    connector.role == CONNECTOR_ROLE
        && client.role == CLIENT_ROLE
        && connector.trust_domain == client.trust_domain
}

#[cfg(test)]
mod tests {
    use super::*;

    const ID: &str = "550e8400-e29b-41d4-a716-446655440000";

    #[test]
    fn validates_connector() {
        assert!(validate_connector_spiffe(&format!(
            "spiffe://workspace-a.example/connector/{ID}"
        )));
    }

    #[test]
    fn validates_client() {
        assert!(validate_client_spiffe(&format!(
            "spiffe://workspace-a.example/client/{ID}"
        )));
    }

    #[test]
    fn validates_relay() {
        let uri = crate::appmeta::relay_spiffe_id(ID);

        assert!(validate_relay_spiffe(&uri).is_some());
    }

    #[test]
    fn rejects_wrong_relay_domain() {
        let uri = format!("spiffe://workspace-a.zecurity.in/relay/{ID}");

        assert!(validate_relay_spiffe(&uri).is_none());
    }

    #[test]
    fn rejects_connector_as_relay() {
        let uri = format!("spiffe://zecurity.in/connector/{ID}");

        assert!(validate_relay_spiffe(&uri).is_none());
    }

    #[test]
    fn rejects_client_as_relay() {
        let uri = format!("spiffe://zecurity.in/client/{ID}");

        assert!(validate_relay_spiffe(&uri).is_none());
    }

    #[test]
    fn rejects_wrong_role() {
        assert!(!validate_connector_spiffe(&format!(
            "spiffe://workspace-a.example/shield/{ID}"
        )));
    }

    #[test]
    fn rejects_malformed_identity() {
        assert!(!validate_connector_spiffe(""));
        assert!(!validate_connector_spiffe(
            "spiffe://workspace-a.example/connector/not-a-uuid"
        ));
    }

    #[test]
    fn compares_workspaces() {
        let connector = format!("spiffe://workspace-a.example/connector/{ID}");
        let client = format!("spiffe://workspace-a.example/client/{ID}");
        let other_client = format!("spiffe://workspace-b.example/client/{ID}");

        assert!(same_workspace(&connector, &client));
        assert!(!same_workspace(&connector, &other_client));
    }
}
