use anyhow::Result;
use rcgen::{
    CertificateParams, DistinguishedName, DnType, KeyPair, SanType, PKCS_ECDSA_P384_SHA384,
};
use std::net::IpAddr;

pub struct RelayCsr {
    pub private_key_pem: String,
    pub csr_der: Vec<u8>,
}

pub fn generate_relay_csr(
    relay_id: &str,
    dns_sans: &[String],
    ip_sans: &[IpAddr],
) -> Result<RelayCsr> {
    let key_pair = KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384)?;
    let csr = relay_csr_params(relay_id, dns_sans, ip_sans)?.serialize_request(&key_pair)?;

    Ok(RelayCsr {
        private_key_pem: key_pair.serialize_pem(),
        csr_der: csr.der().to_vec(),
    })
}

/// Build a renewal CSR signed by the Relay's EXISTING private key (D-19).
/// The key is only read, never regenerated or rewritten.
pub fn relay_csr_from_key(
    relay_id: &str,
    key_pem: &[u8],
    dns_sans: &[String],
    ip_sans: &[IpAddr],
) -> Result<Vec<u8>> {
    let key_pem = std::str::from_utf8(key_pem)
        .map_err(|_| anyhow::anyhow!("Relay private key PEM is not UTF-8"))?;
    let key_pair = KeyPair::from_pem(key_pem)?;
    let csr = relay_csr_params(relay_id, dns_sans, ip_sans)?.serialize_request(&key_pair)?;
    Ok(csr.der().to_vec())
}

fn relay_csr_params(
    relay_id: &str,
    dns_sans: &[String],
    ip_sans: &[IpAddr],
) -> Result<CertificateParams> {
    let spiffe_uri = crate::appmeta::relay_spiffe_id(relay_id);
    let mut params = CertificateParams::default();
    let mut dn = DistinguishedName::new();

    dn.push(DnType::CommonName, format!("relay-{relay_id}"));
    params.distinguished_name = dn;
    params
        .subject_alt_names
        .push(SanType::URI(spiffe_uri.try_into()?));
    for dns_name in dns_sans {
        params
            .subject_alt_names
            .push(SanType::DnsName(dns_name.as_str().try_into()?));
    }
    for ip_address in ip_sans {
        params
            .subject_alt_names
            .push(SanType::IpAddress(*ip_address));
    }
    Ok(params)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn generates_relay_private_key_and_der_csr() {
        let relay_id = "550e8400-e29b-41d4-a716-446655440000";

        let relay_csr =
            generate_relay_csr(relay_id, &[], &[]).expect("relay CSR should be generated");

        assert!(relay_csr.private_key_pem.contains("BEGIN PRIVATE KEY"));
        assert!(!relay_csr.csr_der.is_empty());
    }

    #[test]
    fn renewal_csr_reuses_the_existing_key() {
        use x509_parser::certification_request::X509CertificationRequest;
        use x509_parser::prelude::FromDer;

        let relay_id = "550e8400-e29b-41d4-a716-446655440000";
        let existing = KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap();
        let key_pem = existing.serialize_pem();

        let csr_der = relay_csr_from_key(
            relay_id,
            key_pem.as_bytes(),
            &["relay.example.com".to_owned()],
            &["203.0.113.10".parse().unwrap()],
        )
        .unwrap();

        let (_, csr) = X509CertificationRequest::from_der(&csr_der).unwrap();
        csr.verify_signature()
            .expect("CSR must be signed by the existing key");
        assert_eq!(
            csr.certification_request_info.subject_pki.raw,
            existing.public_key_der().as_slice(),
            "renewal CSR must carry the existing public key"
        );
        // Deterministic identity: SPIFFE URI SAN is present.
        let sans = csr
            .requested_extensions()
            .into_iter()
            .flatten()
            .find_map(|ext| match ext {
                x509_parser::extensions::ParsedExtension::SubjectAlternativeName(san) => {
                    Some(san.clone())
                }
                _ => None,
            })
            .expect("CSR has a SAN extension");
        assert!(sans.general_names.iter().any(|name| matches!(
            name,
            x509_parser::extensions::GeneralName::URI(uri) if *uri == crate::appmeta::relay_spiffe_id(relay_id)
        )));
    }
}
