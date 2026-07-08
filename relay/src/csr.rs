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
    let spiffe_uri = crate::appmeta::relay_spiffe_id(relay_id);
    let key_pair = KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384)?;

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

    let csr = params.serialize_request(&key_pair)?;

    Ok(RelayCsr {
        private_key_pem: key_pair.serialize_pem(),
        csr_der: csr.der().to_vec(),
    })
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
}
