// peer_list.rs — D21-R (a2): the controller-signed peer list.
//
// The Shield's peer Connectors used to come from whichever Connector it was
// attached to, so a hostile Connector could replace them with itself, save
// that to state.json, and strand the Shield once its certificate expired.
// The controller now signs each Shield's list with the Workspace CA key; a
// Connector can withhold or replay a list, never forge or alter one.
//
// Signature: ECDSA P-384 / SHA-384 (ASN.1 DER) over
//   PEER_LIST_CONTEXT || payload
// where payload is a serialized PeerListPayload. Must match the controller's
// pki.ShieldPeerListContext / ShieldPeerListDigest byte for byte.

use std::path::Path;

use anyhow::{bail, Context, Result};
use prost::Message;
use ring::signature::{UnparsedPublicKey, ECDSA_P384_SHA384_ASN1};
use x509_parser::prelude::*;

use crate::proto::{PeerListPayload, ResourceSetPayload, SignedPeerList, SignedResourceSet};

/// Domain separation from everything else the Workspace CA key signs
/// (certificates and CRLs are DER SEQUENCEs, first byte 0x30).
pub const PEER_LIST_CONTEXT: &[u8] = b"zecurity/shield-peer-list/v1\0";

/// The Workspace CA public key (uncompressed EC point) from the stored
/// `workspace_ca.crt` bundle, chosen by role (`crl::crl_issuers`).
pub fn workspace_ca_key(state_dir: &str) -> Result<Vec<u8>> {
    let path = Path::new(state_dir).join("workspace_ca.crt");
    let bundle =
        std::fs::read(&path).with_context(|| format!("failed to read {}", path.display()))?;
    workspace_ca_key_from_bundle(&bundle)
}

pub fn workspace_ca_key_from_bundle(bundle_pem: &[u8]) -> Result<Vec<u8>> {
    let issuers =
        crate::crl::crl_issuers(bundle_pem).context("Workspace CA from the stored bundle")?;
    let mut reader = issuers.workspace_ca_pem.as_slice();
    let der = rustls_pemfile::certs(&mut reader)
        .next()
        .context("Workspace CA PEM is empty")?
        .context("Workspace CA PEM")?;
    let (_, cert) = parse_x509_certificate(der.as_ref())
        .map_err(|e| anyhow::anyhow!("Workspace CA parse error: {e:?}"))?;
    Ok(cert.public_key().subject_public_key.data.to_vec())
}

/// Checks `signature` over PEER_LIST_CONTEXT || `payload` with `ca_key`.
pub fn verify_signature(ca_key: &[u8], payload: &[u8], signature: &[u8]) -> Result<()> {
    verify_in_context(ca_key, PEER_LIST_CONTEXT, payload, signature)
        .context("peer list signature does not verify against the Workspace CA")
}

/// Checks `signature` over `context` || `payload` with `ca_key`.
fn verify_in_context(
    ca_key: &[u8],
    context: &[u8],
    payload: &[u8],
    signature: &[u8],
) -> Result<()> {
    let mut message = Vec::with_capacity(context.len() + payload.len());
    message.extend_from_slice(context);
    message.extend_from_slice(payload);
    UnparsedPublicKey::new(&ECDSA_P384_SHA384_ASN1, ca_key)
        .verify(&message, signature)
        .map_err(|_| anyhow::anyhow!("bad signature"))
}

/// D21-R (S): the context of a signed resource set (controller
/// pki.ShieldResourceSetContext).
pub const RESOURCE_SET_CONTEXT: &[u8] = b"zecurity/shield-resource-set/v1\0";

/// D21-R (S): verifies a signed resource set with the Workspace CA key and
/// returns its payload when it is for this Shield. The Shield applies
/// resources only from such a payload.
pub fn verify_resource_set(
    signed: &SignedResourceSet,
    ca_key: &[u8],
    trust_domain: &str,
    shield_id: &str,
) -> Result<ResourceSetPayload> {
    verify_in_context(
        ca_key,
        RESOURCE_SET_CONTEXT,
        &signed.payload,
        &signed.signature,
    )
    .context("resource set signature does not verify against the Workspace CA")?;
    let payload =
        ResourceSetPayload::decode(signed.payload.as_slice()).context("resource set payload")?;
    if payload.trust_domain != trust_domain {
        bail!(
            "resource set is for trust domain {:?}, not {trust_domain:?}",
            payload.trust_domain
        );
    }
    if payload.shield_id != shield_id {
        bail!(
            "resource set is for shield {:?}, not {shield_id:?}",
            payload.shield_id
        );
    }
    Ok(payload)
}

/// Verifies `signed` with the Workspace CA key and returns its payload when
/// it is for this Shield (`trust_domain`, `shield_id`).
pub fn verify(
    signed: &SignedPeerList,
    ca_key: &[u8],
    trust_domain: &str,
    shield_id: &str,
) -> Result<PeerListPayload> {
    verify_signature(ca_key, &signed.payload, &signed.signature)?;
    let payload =
        PeerListPayload::decode(signed.payload.as_slice()).context("peer list payload")?;
    if payload.trust_domain != trust_domain {
        bail!(
            "peer list is for trust domain {:?}, not {trust_domain:?}",
            payload.trust_domain
        );
    }
    if payload.shield_id != shield_id {
        bail!(
            "peer list is for shield {:?}, not {shield_id:?}",
            payload.shield_id
        );
    }
    Ok(payload)
}

#[cfg(test)]
mod tests {
    use super::*;
    use rcgen::{
        BasicConstraints, CertificateParams, CertifiedIssuer, DistinguishedName, DnType, IsCa,
        KeyIdMethod, KeyPair, KeyUsagePurpose, PKCS_ECDSA_P256_SHA256, PKCS_ECDSA_P384_SHA384,
    };

    /// Signed by the controller's Go code (pki.ShieldPeerListDigest +
    /// ecdsa.SignASN1, P-384): the Shield computes the same digest.
    const GO_PUB: &str = "04ef3e5427e817e6646979e1a8705ea2e682c0fae452e023ab4f6958ebf50dadb79d104032212f2869af986b1db6ead3ef8ad381891421225d884a464119f054691a1235b5674eb411da00e8f64526d454eafcc253f2185ff6643f4f4110155a30";
    const GO_PAYLOAD: &str = "643231722063726f73732d6c616e677561676520766563746f72";
    const GO_SIG: &str = "3066023100f05054b6bc131fc7783a627c33626bcb178498efad2bf0d54826cbec33197ca14a8a72686ee3377c844b35031a229318023100dc25ee0e707fcafc6f99e9b73278bfb48e96764c1ffb3e1d16f485c3c8d45cb7b1b8299daa2038f809f60631cc208e61";

    #[test]
    fn a_signature_made_by_the_controller_verifies() {
        let key = hex::decode(GO_PUB).unwrap();
        let payload = hex::decode(GO_PAYLOAD).unwrap();
        let sig = hex::decode(GO_SIG).unwrap();
        verify_signature(&key, &payload, &sig).expect("Go-signed vector");
        let mut other = payload.clone();
        other.push(b'!');
        assert!(verify_signature(&key, &other, &sig).is_err());
    }

    /// Signed by the controller's Go code over pki.ShieldResourceSetDigest:
    /// the resource-set context matches byte for byte, and a resource-set
    /// signature is not a peer-list one.
    const GO_SET_PUB: &str = "0406275693015a6efc75797184ef6049f0ce2be0b3fb39782daa8cfd37e6d6c3e57ef5232d0e1c9b9c264f0b5b9ab93dee4262b7352cd1642691e4721d7a570226c9fdd952f2021f4cd8f31ece2645a2dfa39df9f66db6c708f5c3d1572e0ecf20";
    const GO_SET_PAYLOAD: &str = "64323172207265736f757263652d73657420766563746f72";
    const GO_SET_SIG: &str = "3066023100ea57f4477c0151c54035402776fdd07b85d7a78988a79cd182085d19df2de741605dca7b7395f4c018fc8768c694443c023100f394e3c6ebdbb47f6fa75c65424b77b48b995ca90a0412c745a82231009afbc814485138086053408b781bd485c67612";

    #[test]
    fn a_resource_set_signature_made_by_the_controller_verifies() {
        let key = hex::decode(GO_SET_PUB).unwrap();
        let payload = hex::decode(GO_SET_PAYLOAD).unwrap();
        let sig = hex::decode(GO_SET_SIG).unwrap();
        verify_in_context(&key, RESOURCE_SET_CONTEXT, &payload, &sig).expect("Go-signed vector");
        assert!(
            verify_signature(&key, &payload, &sig).is_err(),
            "not a peer list"
        );
    }

    fn ca_params(cn: &str, key_id: &[u8], constraint: BasicConstraints) -> CertificateParams {
        let mut params = CertificateParams::default();
        let mut dn = DistinguishedName::new();
        dn.push(DnType::CommonName, cn);
        params.distinguished_name = dn;
        params.is_ca = IsCa::Ca(constraint);
        params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
        params.key_identifier_method = KeyIdMethod::PreSpecified(key_id.to_vec());
        params
    }

    /// The key comes from the Workspace CA, whichever order the bundle has.
    #[test]
    fn the_workspace_ca_key_is_chosen_by_role() {
        let root = CertifiedIssuer::self_signed(
            ca_params("Platform Root", b"root", BasicConstraints::Constrained(2)),
            KeyPair::generate_for(&PKCS_ECDSA_P256_SHA256).unwrap(),
        )
        .unwrap();
        let intermediate = CertifiedIssuer::signed_by(
            ca_params(
                "Platform Intermediate",
                b"int",
                BasicConstraints::Constrained(1),
            ),
            KeyPair::generate_for(&PKCS_ECDSA_P256_SHA256).unwrap(),
            &root,
        )
        .unwrap();
        let ws_key = KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap();
        let ws_pub = ws_key.public_key_raw().to_vec();
        let workspace = CertifiedIssuer::signed_by(
            ca_params("workspace-ws-test", b"ws", BasicConstraints::Constrained(0)),
            ws_key,
            &intermediate,
        )
        .unwrap();
        for bundle in [
            format!("{}{}", workspace.pem(), intermediate.pem()),
            format!("{}{}", intermediate.pem(), workspace.pem()),
        ] {
            assert_eq!(
                workspace_ca_key_from_bundle(bundle.as_bytes()).unwrap(),
                ws_pub
            );
        }
        assert!(workspace_ca_key_from_bundle(workspace.pem().as_bytes()).is_err());
    }
}
