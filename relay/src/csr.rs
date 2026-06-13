use anyhow::Result;
use rcgen::{CertificateParams, DistinguishedName, DnType, KeyPair, SanType};

pub struct RelayCsr {
    pub private_key_pem: String,
    pub csr_pem: String,
}

pub fn generate_relay_csr(relay_id: &str) -> Result<RelayCsr> {
    let spiffe_uri = crate::appmeta::relay_spiffe_id(relay_id);

    let key_pair = KeyPair::generate()?;

    let private_key_pem = key_pair.serialize_pem();

    let mut params = CertificateParams::default();

    let mut dn = DistinguishedName::new();

    dn.push(DnType::CommonName, format!("relay-{}", relay_id));

    params.distinguished_name = dn;
    params
        .subject_alt_names
        .push(SanType::URI(spiffe_uri.clone().try_into()?));

    let csr_pem = params.serialize_request(&key_pair)?.pem()?;

    Ok(RelayCsr {
        private_key_pem,
        csr_pem,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn generates_relay_private_key_and_csr() {
        let relay_id = "550e8400-e29b-41d4-a716-446655440000";

        let relay_csr = generate_relay_csr(relay_id).expect("relay CSR should be generated");

        assert!(relay_csr.private_key_pem.contains("BEGIN PRIVATE KEY"));
        assert!(relay_csr.csr_pem.contains("BEGIN CERTIFICATE REQUEST"));
    }
}
