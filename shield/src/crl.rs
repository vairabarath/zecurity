//! Workspace CA selection and (D21-R A′) workspace CRL checks for the Shield.
//!
//! A copy of the Connector's `crl.rs` issuer logic (certificate audit bug 6),
//! with the same test vectors: the stored `workspace_ca.crt` bundle holds the
//! Workspace CA and the Platform Intermediate, and the roles are decided by
//! their relationship, never by their order. D21-R (a2) verifies the
//! controller-signed peer list with the Workspace CA key chosen here.

use x509_parser::prelude::*;

/// The two CAs in the stored bundle, by role.
#[derive(Clone, Debug)]
pub struct CrlIssuers {
    /// Signs `/ca.crl` (revoked devices and connectors) and, with a context
    /// prefix, the Shield's peer list (D21-R a2).
    pub workspace_ca_pem: Vec<u8>,
    /// Signs `/relay.crl` (revoked relays).
    #[allow(dead_code)]
    pub intermediate_pem: Vec<u8>,
}

/// Derives the two CRL issuers from the stored CA bundle by their actual
/// relationship, not their position: exactly two distinct, currently valid CA
/// certificates that may sign certificates and CRLs, where one (the Workspace
/// CA) is issued by the other (the Platform Intermediate) and not the reverse.
/// The Intermediate must not be self-signed (a root does not belong here) and
/// must be allowed to issue a CA. Anything else is rejected; callers then
/// leave revocation state unavailable, so checks deny.
///
/// The bundle itself was anchored in the platform root at enrollment and
/// renewal; this only decides which of its two certificates signs which CRL.
pub fn crl_issuers(bundle_pem: &[u8]) -> anyhow::Result<CrlIssuers> {
    let ders = issuer_certificates(bundle_pem, "CA bundle")?;
    let [first_der, second_der] = ders.as_slice() else {
        anyhow::bail!(
            "CA bundle must be exactly the Workspace CA and the Platform Intermediate, got {} certificates",
            ders.len()
        );
    };
    if first_der.as_ref() == second_der.as_ref() {
        anyhow::bail!("CA bundle contains the same certificate twice");
    }
    let first = parse_crl_signing_ca(first_der.as_ref(), "first CA bundle certificate")?;
    let second = parse_crl_signing_ca(second_der.as_ref(), "second CA bundle certificate")?;
    if first.subject() == second.subject() {
        anyhow::bail!("CA bundle certificates have the same subject");
    }
    let (workspace_der, workspace, intermediate_der, intermediate) =
        match (issued_by(&first, &second), issued_by(&second, &first)) {
            (true, false) => (first_der, first, second_der, second),
            (false, true) => (second_der, second, first_der, first),
            (true, true) => anyhow::bail!("CA bundle certificates certify each other"),
            (false, false) => {
                anyhow::bail!("CA bundle is not a Workspace CA issued by the Platform Intermediate")
            }
        };
    if issued_by(&intermediate, &intermediate) {
        anyhow::bail!("Platform Intermediate must not be self-signed");
    }
    if issued_by(&workspace, &workspace) {
        anyhow::bail!("Workspace CA must not be self-signed");
    }
    let path_len = intermediate
        .basic_constraints()
        .map_err(|e| anyhow::anyhow!("Platform Intermediate basic constraints: {e:?}"))?
        .and_then(|bc| bc.value.path_len_constraint);
    if path_len == Some(0) {
        anyhow::bail!("Platform Intermediate may not issue CA certificates (path length 0)");
    }
    Ok(CrlIssuers {
        workspace_ca_pem: pem_from_der(workspace_der.as_ref()),
        intermediate_pem: pem_from_der(intermediate_der.as_ref()),
    })
}

fn issuer_certificates(
    pem: &[u8],
    label: &str,
) -> anyhow::Result<Vec<rustls::pki_types::CertificateDer<'static>>> {
    let mut reader = pem;
    rustls_pemfile::certs(&mut reader)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|e| anyhow::anyhow!("failed to parse {label}: {e}"))
}

/// A currently valid CA certificate allowed to sign certificates and CRLs.
fn parse_crl_signing_ca<'a>(der: &'a [u8], label: &str) -> anyhow::Result<X509Certificate<'a>> {
    let (remaining, cert) =
        parse_x509_certificate(der).map_err(|e| anyhow::anyhow!("{label} parse error: {e:?}"))?;
    if !remaining.is_empty() {
        anyhow::bail!("trailing data after {label}");
    }
    if !cert.is_ca() {
        anyhow::bail!("{label} is not a CA certificate");
    }
    let key_usage = cert
        .key_usage()
        .map_err(|e| anyhow::anyhow!("{label} key usage: {e:?}"))?
        .ok_or_else(|| anyhow::anyhow!("{label} has no key usage"))?;
    if !key_usage.value.key_cert_sign() || !key_usage.value.crl_sign() {
        anyhow::bail!("{label} may not sign certificates and CRLs");
    }
    if !cert.validity().is_valid() {
        anyhow::bail!("{label} is outside its validity period");
    }
    Ok(cert)
}

/// `child` names `issuer` as its issuer and carries its signature.
fn issued_by(child: &X509Certificate<'_>, issuer: &X509Certificate<'_>) -> bool {
    child.issuer() == issuer.subject() && child.verify_signature(Some(issuer.public_key())).is_ok()
}

fn pem_from_der(der: &[u8]) -> Vec<u8> {
    use base64::Engine as _;
    let body = base64::engine::general_purpose::STANDARD.encode(der);
    let mut pem = String::from("-----BEGIN CERTIFICATE-----\n");
    for line in body.as_bytes().chunks(64) {
        pem.push_str(std::str::from_utf8(line).expect("base64 is ASCII"));
        pem.push('\n');
    }
    pem.push_str("-----END CERTIFICATE-----\n");
    pem.into_bytes()
}
