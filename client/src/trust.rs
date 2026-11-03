//! Certificate audit bug 1 (client): where the client's trust comes from.
//!
//! The anchor is the platform root CA compiled into this binary (see
//! build.rs), never anything downloaded. `/ca.crt` (plain HTTP) is accepted
//! only if it is exactly one Platform Intermediate issued by that root; the
//! EnrollDevice and RenewCert replies are validated before anything is saved;
//! and the saved chain (Workspace CA + Intermediate) is checked against the
//! root whenever it is loaded from disk.
//!
//! Every function takes the root (or the saved anchors) as an argument, so
//! tests can supply their own PKI; only [`embedded_platform_root_pem`] reads
//! the compiled-in one.

use anyhow::{anyhow, bail, Context, Result};
use base64::Engine;
use rustls::pki_types::CertificateDer;
use rustls_pemfile::certs;
use x509_parser::prelude::*;

include!(concat!(env!("OUT_DIR"), "/platform_root.rs"));

/// The platform root CA compiled into this build.
pub fn embedded_platform_root_pem() -> Result<&'static str> {
    EMBEDDED_PLATFORM_ROOT_PEM.ok_or_else(|| {
        anyhow!(
            "this zecurity-client build has no platform root CA, so it cannot verify the \
             controller; rebuild with ZECURITY_PLATFORM_ROOT_PEM_FILE (client/scripts/dev-build.sh)"
        )
    })
}

/// PEM for a single DER certificate.
pub fn pem_from_der(der: &CertificateDer<'_>) -> String {
    let body = base64::engine::general_purpose::STANDARD.encode(der.as_ref());
    let mut pem = String::from("-----BEGIN CERTIFICATE-----\n");
    for line in body.as_bytes().chunks(64) {
        pem.push_str(std::str::from_utf8(line).expect("base64 is ASCII"));
        pem.push('\n');
    }
    pem.push_str("-----END CERTIFICATE-----\n");
    pem
}

fn all_certificates(pem: &[u8], label: &str) -> Result<Vec<CertificateDer<'static>>> {
    certs(&mut &pem[..])
        .collect::<std::result::Result<Vec<_>, _>>()
        .with_context(|| format!("parse {label}"))
}

/// Exactly one certificate in `pem`.
pub fn single_certificate(pem: &[u8], label: &str) -> Result<CertificateDer<'static>> {
    let certificates = all_certificates(pem, label)?;
    match <[_; 1]>::try_from(certificates) {
        Ok([certificate]) => Ok(certificate),
        Err(certificates) => bail!(
            "{label} must be exactly one certificate, got {}",
            certificates.len()
        ),
    }
}

fn parse<'a>(der: &'a CertificateDer<'_>, label: &str) -> Result<X509Certificate<'a>> {
    X509Certificate::from_der(der.as_ref())
        .map(|(_, cert)| cert)
        .map_err(|e| anyhow!("parse {label}: {e:?}"))
}

/// `child` is a CA certificate issued (name and signature) by `issuer`, and
/// currently valid.
fn require_ca_issued_by(
    child: &X509Certificate<'_>,
    issuer: &X509Certificate<'_>,
    what: &str,
) -> Result<()> {
    if !child.is_ca() {
        bail!("{what} is not a CA certificate");
    }
    if child.issuer() != issuer.subject() {
        bail!("{what} was not issued by {}", issuer.subject());
    }
    child
        .verify_signature(Some(issuer.public_key()))
        .map_err(|e| anyhow!("{what} signature invalid: {e:?}"))?;
    if !child.validity().is_valid() {
        bail!("{what} is outside its validity period");
    }
    Ok(())
}

/// `/ca.crt` must be exactly one certificate: a Platform Intermediate issued
/// by `root_pem`. Returns it — the only certificate the login connection
/// trusts. (Before, the download was trusted wholesale, with no pin.)
pub fn pinned_intermediate(download_pem: &str, root_pem: &str) -> Result<CertificateDer<'static>> {
    let root_der = single_certificate(root_pem.as_bytes(), "platform root CA")?;
    let root = parse(&root_der, "platform root CA")?;
    let certificates = all_certificates(download_pem.as_bytes(), "/ca.crt")?;
    let [intermediate_der] = certificates.as_slice() else {
        bail!(
            "/ca.crt must contain exactly one certificate (the Platform Intermediate), got {} — possible MITM, aborting",
            certificates.len()
        );
    };
    if intermediate_der.as_ref() == root_der.as_ref() {
        bail!("/ca.crt is the platform root itself, not the Intermediate — aborting");
    }
    let intermediate = parse(intermediate_der, "/ca.crt")?;
    require_ca_issued_by(&intermediate, &root, "/ca.crt Intermediate")
        .context("/ca.crt is not the platform's Intermediate — possible MITM, aborting")?;
    Ok(intermediate_der.clone().into_owned())
}

/// The anchors a returned chain is checked against.
pub struct Anchors<'a> {
    /// The pinned Platform Intermediate.
    pub intermediate: &'a CertificateDer<'a>,
    /// On renewal, the Workspace CA saved at enrollment; the returned one must
    /// be identical. `None` at enrollment (it is learned then).
    pub workspace_ca: Option<&'a CertificateDer<'a>>,
}

/// A returned (Workspace CA, Intermediate, leaf) must chain pinned
/// Intermediate → Workspace CA → leaf, and the leaf must be this device's:
/// exactly `expected_spiffe_id`, client authentication, the key in the CSR
/// this client sent.
pub fn validate_issued_chain(
    anchors: &Anchors<'_>,
    workspace_ca_pem: &str,
    intermediate_ca_pem: &str,
    leaf_pem: &str,
    expected_spiffe_id: &str,
    key_spki_der: &[u8],
) -> Result<()> {
    let returned_intermediate =
        single_certificate(intermediate_ca_pem.as_bytes(), "returned Intermediate CA")?;
    if returned_intermediate.as_ref() != anchors.intermediate.as_ref() {
        bail!("returned Intermediate CA is not the pinned Platform Intermediate");
    }
    let intermediate = parse(anchors.intermediate, "pinned Intermediate CA")?;

    let workspace_der = single_certificate(workspace_ca_pem.as_bytes(), "returned Workspace CA")?;
    if let Some(saved) = anchors.workspace_ca {
        if workspace_der.as_ref() != saved.as_ref() {
            bail!("returned Workspace CA is not the saved Workspace CA");
        }
    }
    let workspace = parse(&workspace_der, "returned Workspace CA")?;
    require_ca_issued_by(&workspace, &intermediate, "returned Workspace CA")?;

    let leaf_der = single_certificate(leaf_pem.as_bytes(), "returned certificate")?;
    let leaf = parse(&leaf_der, "returned certificate")?;
    if leaf.is_ca() {
        bail!("returned certificate must not be a CA");
    }
    if leaf.issuer() != workspace.subject() {
        bail!("returned certificate was not issued by the returned Workspace CA");
    }
    leaf.verify_signature(Some(workspace.public_key()))
        .map_err(|e| anyhow!("returned certificate signature invalid: {e:?}"))?;
    if !leaf.validity().is_valid() {
        bail!("returned certificate is outside its validity period");
    }
    let uris: Vec<&str> = leaf
        .subject_alternative_name()
        .ok()
        .flatten()
        .map(|san| {
            san.value
                .general_names
                .iter()
                .filter_map(|name| match name {
                    GeneralName::URI(uri) => Some(*uri),
                    _ => None,
                })
                .collect()
        })
        .unwrap_or_default();
    if uris != [expected_spiffe_id] {
        bail!("returned certificate identity {uris:?} is not {expected_spiffe_id}");
    }
    let eku = leaf
        .extended_key_usage()
        .ok()
        .flatten()
        .context("returned certificate has no extended key usage")?;
    if !eku.value.client_auth {
        bail!("returned certificate must allow client authentication");
    }
    if leaf.public_key().raw != key_spki_der {
        bail!("returned certificate is not for this device's key");
    }
    Ok(())
}

/// The saved chain "Workspace CA + Intermediate" (exactly those two) must be
/// anchored in `root_pem`: the Intermediate issued by the root, the Workspace
/// CA issued by the Intermediate. Returns (Workspace CA, Intermediate).
pub fn saved_anchors(
    ca_cert_pem: &str,
    root_pem: &str,
) -> Result<(CertificateDer<'static>, CertificateDer<'static>)> {
    let certificates = all_certificates(ca_cert_pem.as_bytes(), "saved CA chain")?;
    let [workspace_der, intermediate_der] = certificates.as_slice() else {
        bail!(
            "saved CA chain must be the Workspace CA and the Intermediate, got {} certificates",
            certificates.len()
        );
    };
    pinned_intermediate(&pem_from_der(intermediate_der), root_pem)
        .context("saved Intermediate is not issued by the platform root")?;
    let intermediate = parse(intermediate_der, "saved Intermediate")?;
    let workspace = parse(workspace_der, "saved Workspace CA")?;
    require_ca_issued_by(&workspace, &intermediate, "saved Workspace CA")?;
    Ok((
        workspace_der.clone().into_owned(),
        intermediate_der.clone().into_owned(),
    ))
}

/// The saved anchors without the root check, for renewal: they were
/// validated when saved and again when loaded. Returns (Workspace CA,
/// Intermediate).
pub fn split_saved_chain(
    ca_cert_pem: &str,
) -> Result<(CertificateDer<'static>, CertificateDer<'static>)> {
    let certificates = all_certificates(ca_cert_pem.as_bytes(), "saved CA chain")?;
    match <[_; 2]>::try_from(certificates) {
        Ok([workspace, intermediate]) => Ok((workspace, intermediate)),
        Err(c) => bail!(
            "saved CA chain must be the Workspace CA and the Intermediate, got {} certificates",
            c.len()
        ),
    }
}

/// The SubjectPublicKeyInfo (DER) of the key in `csr_pem` — the key the
/// returned certificate must carry. Taken from the CSR, it is the same for
/// software and TPM-backed keys.
pub fn csr_key_spki(csr_pem: &str) -> Result<Vec<u8>> {
    let (_, pem) = x509_parser::pem::parse_x509_pem(csr_pem.as_bytes())
        .map_err(|e| anyhow!("parse CSR PEM: {e:?}"))?;
    let (_, csr) =
        x509_parser::certification_request::X509CertificationRequest::from_der(&pem.contents)
            .map_err(|e| anyhow!("parse CSR: {e:?}"))?;
    Ok(csr.certification_request_info.subject_pki.raw.to_vec())
}

/// The SPIFFE ID the controller assigns a device:
/// `spiffe://<trust domain>/client/<device id>`.
pub fn require_device_spiffe_id(spiffe_id: &str, device_id: &str) -> Result<()> {
    let ok = !device_id.is_empty()
        && spiffe_id
            .strip_prefix("spiffe://")
            .and_then(|rest| rest.split_once('/'))
            .is_some_and(|(domain, path)| {
                !domain.is_empty() && path == format!("client/{device_id}")
            });
    if !ok {
        bail!("returned SPIFFE ID {spiffe_id:?} is not a device ID for {device_id:?}");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::test_pki::{
        device_key_and_csr, device_spiffe_id, other_key_csr, AttackerPki, TestPki, DEVICE_ID,
    };

    fn der(pem: &str) -> CertificateDer<'static> {
        single_certificate(pem.as_bytes(), "test").unwrap()
    }

    #[test]
    fn pinning_requires_exactly_one_intermediate_issued_by_the_root() {
        let pki = TestPki::new();
        let attacker = AttackerPki::new();
        let pinned = pinned_intermediate(&pki.intermediate_pem, &pki.root_pem).expect("genuine");
        assert_eq!(pinned.as_ref(), der(&pki.intermediate_pem).as_ref());

        let appended = format!("{}\n{}", pki.intermediate_pem, attacker.root_pem);
        let err = pinned_intermediate(&appended, &pki.root_pem).unwrap_err();
        assert!(err.to_string().contains("exactly one certificate"), "{err}");
        for bad in [
            attacker.root_pem.as_str(),
            attacker.fake_intermediate_pem.as_str(),
            pki.workspace_ca_pem.as_str(),
            pki.root_pem.as_str(),
            TestPki::new().intermediate_pem.as_str(),
            "",
        ] {
            assert!(pinned_intermediate(bad, &pki.root_pem).is_err());
        }
    }

    fn check(
        pki: &TestPki,
        ws: &str,
        int: &str,
        leaf: &str,
        spiffe: &str,
        spki: &[u8],
    ) -> Result<()> {
        let pinned = der(&pki.intermediate_pem);
        validate_issued_chain(
            &Anchors {
                intermediate: &pinned,
                workspace_ca: None,
            },
            ws,
            int,
            leaf,
            spiffe,
            spki,
        )
    }

    #[test]
    fn issued_chain_validation_rejects_each_broken_link() {
        let pki = TestPki::new();
        let attacker = AttackerPki::new();
        let (_key, csr) = device_key_and_csr();
        let spki = csr_key_spki(&csr).unwrap();
        let id = device_spiffe_id(DEVICE_ID);
        let (ws, int) = (pki.workspace_ca_pem.as_str(), pki.intermediate_pem.as_str());
        let genuine = pki.sign_device_csr(&csr, DEVICE_ID);
        check(&pki, ws, int, &genuine, &id, &spki).expect("genuine chain");

        let err = |r: Result<()>| r.expect_err("must be rejected").to_string();
        assert!(err(check(
            &pki,
            ws,
            &attacker.fake_intermediate_pem,
            &genuine,
            &id,
            &spki
        ))
        .contains("pinned Platform Intermediate"));
        assert!(err(check(
            &pki,
            &attacker.workspace_ca_pem,
            int,
            &genuine,
            &id,
            &spki
        ))
        .contains("Workspace CA"));
        let other = TestPki::new();
        assert!(err(check(
            &pki,
            &other.workspace_ca_pem,
            int,
            &genuine,
            &id,
            &spki
        ))
        .contains("Workspace CA"));
        assert!(err(check(
            &pki,
            ws,
            int,
            &attacker.sign_device_csr(&csr, DEVICE_ID),
            &id,
            &spki
        ))
        // Mallory's Workspace CA carries the genuine one's name, so the
        // signature check is what refuses it.
        .contains("signature invalid"));
        assert!(err(check(
            &pki,
            ws,
            int,
            &pki.sign_device_csr(&other_key_csr(), DEVICE_ID),
            &id,
            &spki
        ))
        .contains("this device's key"));
        assert!(err(check(
            &pki,
            ws,
            int,
            &genuine,
            &device_spiffe_id("other"),
            &spki
        ))
        .contains("identity"));
        let two = format!("{ws}\n{}", attacker.root_pem);
        assert!(err(check(&pki, &two, int, &genuine, &id, &spki)).contains("exactly one"));
        let two_leaf = format!("{genuine}\n{ws}");
        assert!(err(check(&pki, ws, int, &two_leaf, &id, &spki)).contains("exactly one"));
    }

    #[test]
    fn issued_chain_validation_rejects_wrong_shape_certificates() {
        let pki = TestPki::new();
        let (_key, csr) = device_key_and_csr();
        let spki = csr_key_spki(&csr).unwrap();
        let id = device_spiffe_id(DEVICE_ID);
        let (ws, int) = (pki.workspace_ca_pem.as_str(), pki.intermediate_pem.as_str());
        let err = |r: Result<()>| r.expect_err("must be rejected").to_string();

        let server_only = pki.sign_device_csr_shaped(&csr, DEVICE_ID, |p| {
            p.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth];
        });
        assert!(
            err(check(&pki, ws, int, &server_only, &id, &spki)).contains("client authentication")
        );
        let expired = pki.sign_device_csr_shaped(&csr, DEVICE_ID, |p| {
            p.not_before = rcgen::date_time_ymd(2000, 1, 1);
            p.not_after = rcgen::date_time_ymd(2000, 1, 2);
        });
        assert!(err(check(&pki, ws, int, &expired, &id, &spki)).contains("validity"));
        let ca_leaf = pki.sign_device_csr_shaped(&csr, DEVICE_ID, |p| {
            p.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        });
        assert!(err(check(&pki, ws, int, &ca_leaf, &id, &spki)).contains("must not be a CA"));
        let (not_a_ca, leaf) = pki.non_ca_workspace(&csr, DEVICE_ID);
        assert!(err(check(&pki, &not_a_ca, int, &leaf, &id, &spki)).contains("not a CA"));
    }

    #[test]
    fn expired_cas_are_refused() {
        let pki = TestPki::new();
        let (expired_int, expired_ws) = pki.expired_cas();
        let err = pinned_intermediate(&expired_int, &pki.root_pem).unwrap_err();
        assert!(format!("{err:#}").contains("validity"), "{err:#}");
        let saved = format!("{expired_ws}\n{}", pki.intermediate_pem);
        let err = saved_anchors(&saved, &pki.root_pem).unwrap_err();
        assert!(format!("{err:#}").contains("validity"), "{err:#}");
    }

    #[test]
    fn renewal_anchors_must_be_identical() {
        let pki = TestPki::new();
        let (_key, csr) = device_key_and_csr();
        let spki = csr_key_spki(&csr).unwrap();
        let id = device_spiffe_id(DEVICE_ID);
        let (ws, int) = split_saved_chain(&pki.device_ca_chain()).unwrap();
        let anchors = Anchors {
            intermediate: &int,
            workspace_ca: Some(&ws),
        };
        let leaf = pki.sign_device_csr(&csr, DEVICE_ID);
        validate_issued_chain(
            &anchors,
            &pki.workspace_ca_pem,
            &pki.intermediate_pem,
            &leaf,
            &id,
            &spki,
        )
        .expect("same anchors");
        let other = TestPki::new();
        let err = validate_issued_chain(
            &anchors,
            &other.workspace_ca_pem,
            &pki.intermediate_pem,
            &other.sign_device_csr(&csr, DEVICE_ID),
            &id,
            &spki,
        )
        .unwrap_err();
        assert!(err.to_string().contains("saved Workspace CA"), "{err}");
    }

    #[test]
    fn saved_anchors_must_chain_to_the_root() {
        let pki = TestPki::new();
        let attacker = AttackerPki::new();
        let (ws, int) = saved_anchors(&pki.device_ca_chain(), &pki.root_pem).expect("genuine");
        assert_eq!(ws.as_ref(), der(&pki.workspace_ca_pem).as_ref());
        assert_eq!(int.as_ref(), der(&pki.intermediate_pem).as_ref());

        let other = TestPki::new();
        let mixed = format!("{}\n{}", other.workspace_ca_pem, pki.intermediate_pem);
        for bad in [
            other.device_ca_chain(),
            mixed,
            format!(
                "{}\n{}",
                attacker.workspace_ca_pem, attacker.fake_intermediate_pem
            ),
            pki.intermediate_pem.clone(),
            format!("{}\n{}", pki.device_ca_chain(), attacker.root_pem),
            String::new(),
        ] {
            assert!(
                saved_anchors(&bad, &pki.root_pem).is_err(),
                "accepted: {bad}"
            );
        }
    }

    #[test]
    fn csr_key_is_the_generated_key() {
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P384_SHA384).unwrap();
        let csr = rcgen::CertificateParams::default()
            .serialize_request(&key)
            .unwrap()
            .pem()
            .unwrap();
        assert_eq!(csr_key_spki(&csr).unwrap(), key.public_key_der());
    }

    #[test]
    fn device_spiffe_id_format() {
        require_device_spiffe_id(&device_spiffe_id(DEVICE_ID), DEVICE_ID).unwrap();
        for bad in [
            format!("spiffe://ws-test.zecurity.in/connector/{DEVICE_ID}"),
            format!("spiffe://ws-test.zecurity.in/client/{DEVICE_ID}/x"),
            format!("spiffe:///client/{DEVICE_ID}"),
            device_spiffe_id("other"),
            String::new(),
        ] {
            assert!(require_device_spiffe_id(&bad, DEVICE_ID).is_err(), "{bad}");
        }
        assert!(require_device_spiffe_id(&device_spiffe_id(""), "").is_err());
    }
}
