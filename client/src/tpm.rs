//! Hardware-backed device keys via TPM 2.0 (Sprint 19, PENDING-17). Linux-only
//! — see the `[target.'cfg(target_os = "linux")'.dependencies]` gate in
//! Cargo.toml; this module is never compiled on a hypothetical future
//! non-Linux target.
//!
//! `TpmSigner` is the single place a TPM signature is produced, and it is
//! reached through each library's own "key I cannot see" abstraction:
//!
//! - `rcgen::RemoteKeyPair` — "a private key that is not directly accessible,
//!   but can be used to sign messages... for example an HSM" — for CSR
//!   signing at enrollment (`login.rs`) and renewal
//!   (`daemon.rs::build_renewal_csr`). Those call sites are unchanged; only
//!   *how the `KeyPair` is constructed* differs.
//! - `rustls::sign::SigningKey`/`Signer` (via [`TpmSigningKey`]) — for
//!   data-plane mTLS client authentication in `tunnel_pool.rs`/`relay_pool.rs`.
//!
//! In neither direction does a private key cross the boundary: the callers
//! hand over a message and get back a signature.
//!
//! Key material (`TpmKeyMaterial`) is an opaque, TPM-sealed `(public,
//! private)` blob pair — the private half is encrypted by the TPM itself and
//! meaningless without that exact chip. It is `Serialize`/`Deserialize`
//! (`tss_esapi::abstraction::transient::KeyMaterial` derives both), so it
//! stores directly in `state_store.rs` alongside the rest of a device's
//! persisted state — no separate blob format to invent.

use std::convert::TryFrom;
use std::str::FromStr;
use std::sync::{Arc, Mutex};

use anyhow::{anyhow, Result};
use rcgen::{RemoteKeyPair, SignatureAlgorithm};
use sha2::{Digest as _, Sha256};
use tss_esapi::{
    abstraction::transient::{KeyParams, TransientKeyContext},
    interface_types::{algorithm::HashingAlgorithm, ecc::EccCurve},
    structures::{Digest as TpmDigest, EccScheme, EccSignature, HashScheme, Signature as TpmSignature},
    tcti_ldr::{DeviceConfig, TctiNameConf},
    utils::PublicKey as TpmPublicKey,
};

/// Opaque, TPM-sealed key material — safe to persist as-is. Re-exported under
/// this name so callers (state_store.rs, login.rs, daemon.rs) never need to
/// name the tss_esapi type directly.
pub type TpmKeyMaterial = tss_esapi::abstraction::transient::KeyMaterial;

/// The resource-managed TPM device node. Kernel-arbitrated, safe for
/// concurrent userspace access — unlike the raw `/dev/tpm0`, which is meant
/// for a single trusted resource-manager process and would let one TPM
/// client starve every other.
const TPM_DEVICE_PATH: &str = "/dev/tpmrm0";

/// TCG PC Client Platform TPM Profile mandates NIST P-256 support; P-384
/// support is optional and inconsistently implemented across real TPM chips.
/// P-256 is the universally-supported choice for a hardware-backed key —
/// deliberately different from the software path's P-384
/// (`rcgen::PKCS_ECDSA_P384_SHA384` in `login.rs`), since the two key
/// backends have different hardware constraints.
fn key_params() -> KeyParams {
    KeyParams::Ecc {
        curve: EccCurve::NistP256,
        scheme: EccScheme::EcDsa(HashScheme::new(HashingAlgorithm::Sha256)),
    }
}

fn open_context() -> Result<TransientKeyContext> {
    let device = DeviceConfig::from_str(TPM_DEVICE_PATH)
        .map_err(|e| anyhow!("invalid TPM device path {TPM_DEVICE_PATH}: {e}"))?;
    TransientKeyContext::builder()
        .with_tcti(TctiNameConf::Device(device))
        .build()
        .map_err(|e| anyhow!("open TPM context via {TPM_DEVICE_PATH}: {e}"))
}

/// True if a TPM 2.0 resource manager device is present and openable.
///
/// Capability detection only. Enrollment deliberately does NOT route through
/// this: `ZECURITY_TPM_KEYS` is fail-closed, and calling `generate()` directly
/// surfaces *why* the TPM was unusable (absent chip vs. permission denied)
/// instead of collapsing both into `false`. Kept because it is the right
/// predicate for "is this machine TPM-capable" — used today by the TPM tests
/// to skip on hardware-less machines, and the natural basis for reporting
/// hardware-backing status to operators.
#[allow(dead_code)]
pub fn tpm_available() -> bool {
    open_context().is_ok()
}

/// Creates a new TPM-resident ECDSA P-256 key. Returns the key material to
/// persist (state_store.rs) and its raw public key bytes — SEC1 uncompressed
/// point form (`0x04 || X || Y`), the same format rcgen's native EC path
/// produces, so downstream code (e.g. the server-side fingerprint computed
/// over this same encoding — controller `publicKeyFingerprint`) doesn't need
/// to know which backend produced it.
pub fn generate() -> Result<(TpmKeyMaterial, Vec<u8>)> {
    let mut ctx = open_context()?;
    let (key_material, _auth) = ctx
        .create_key(key_params(), 0)
        .map_err(|e| anyhow!("create TPM key: {e}"))?;
    let public_key_raw = encode_public_key(key_material.public())?;
    Ok((key_material, public_key_raw))
}

/// Serializes key material for storage. Returned as an opaque JSON string so
/// `state_store.rs`/`runtime.rs` can persist and carry it around without
/// depending on any `tss_esapi` type — those modules compile on every target,
/// this one is Linux-only, and that boundary should not leak.
pub fn serialize_key_material(key_material: &TpmKeyMaterial) -> Result<String> {
    serde_json::to_string(key_material).map_err(|e| anyhow!("serialize TPM key material: {e}"))
}

/// Inverse of [`serialize_key_material`].
pub fn deserialize_key_material(serialized: &str) -> Result<TpmKeyMaterial> {
    serde_json::from_str(serialized).map_err(|e| anyhow!("parse TPM key material: {e}"))
}

/// Reloads previously-created TPM key material (from persisted state) as an
/// `rcgen::KeyPair`, ready to sign a CSR exactly like a software key would —
/// `build_renewal_csr`/`login.rs`'s CSR-building code neither knows nor
/// cares which backend it's holding.
pub fn load(key_material: TpmKeyMaterial) -> Result<rcgen::KeyPair> {
    let signer = new_signer(key_material)?;
    rcgen::KeyPair::from_remote(Box::new(signer)).map_err(|e| anyhow!("wrap TPM key: {e}"))
}

fn encode_public_key(public: &TpmPublicKey) -> Result<Vec<u8>> {
    match public {
        TpmPublicKey::Ecc { x, y } => {
            let mut point = Vec::with_capacity(1 + x.len() + y.len());
            point.push(0x04); // SEC1 uncompressed point marker
            point.extend_from_slice(x);
            point.extend_from_slice(y);
            Ok(point)
        }
        TpmPublicKey::Rsa(_) => Err(anyhow!("expected an ECC TPM key, got RSA")),
    }
}

/// The TPM-resident key, and the one place a TPM signature is ever produced.
///
/// The context is `Mutex`-wrapped because `TransientKeyContext::sign` needs
/// `&mut self` while both `RemoteKeyPair::sign` and `rustls::sign::Signer::sign`
/// hand out only `&self` — and both trait objects must be `Send + Sync`.
///
/// Two different consumers sign with it and neither can see the private key:
/// `rcgen` (CSR signing at enrollment/renewal, via [`RemoteKeyPair`]) and
/// `rustls` (mTLS client auth on the data plane, via [`TpmSigningKey`]). They
/// want different error types but the identical TPM operation, so the
/// operation lives once in [`TpmSigner::sign_der`] and each trait impl is a
/// thin error-mapping shim over it.
#[derive(Debug)]
pub struct TpmSigner {
    ctx: Mutex<TransientKeyContext>,
    key_material: TpmKeyMaterial,
    public_key_raw: Vec<u8>,
}

impl TpmSigner {
    /// SHA-256 the message, have the TPM sign the digest, and DER-encode the
    /// raw (r, s) it returns.
    ///
    /// Both callers want exactly this: rcgen embeds the result as a CSR's
    /// signature BIT STRING, and rustls sends it as the TLS
    /// CertificateVerify signature for `ECDSA_NISTP256_SHA256` — the same
    /// ECDSA-Sig-Value DER either way. The message arrives unhashed in both
    /// cases (both traits document that hashing is the implementer's job),
    /// and SHA-256 is what `key_params()` baked into the key's own scheme at
    /// creation, so the key cannot be asked for anything else.
    fn sign_der(&self, msg: &[u8]) -> Result<Vec<u8>> {
        let hash = Sha256::digest(msg);
        let digest = TpmDigest::try_from(hash.to_vec())
            .map_err(|e| anyhow!("build TPM digest: {e}"))?;

        let mut ctx = self
            .ctx
            .lock()
            .map_err(|_| anyhow!("TPM context lock poisoned"))?;
        let signature = ctx
            .sign(self.key_material.clone(), key_params(), None, digest)
            .map_err(|e| anyhow!("TPM sign: {e}"))?;

        match signature {
            TpmSignature::EcDsa(ecdsa) => der_encode_ecdsa_signature(&ecdsa),
            other => Err(anyhow!("expected an ECDSA TPM signature, got {other:?}")),
        }
    }
}

impl RemoteKeyPair for TpmSigner {
    fn public_key(&self) -> &[u8] {
        &self.public_key_raw
    }

    fn sign(&self, msg: &[u8]) -> Result<Vec<u8>, rcgen::Error> {
        self.sign_der(msg).map_err(|e| {
            log_tpm_error("CSR signing", &e);
            rcgen::Error::RemoteKeyError
        })
    }

    fn algorithm(&self) -> &'static SignatureAlgorithm {
        &rcgen::PKCS_ECDSA_P256_SHA256
    }
}

/// The TPM key as a `rustls` signing key, for mTLS client authentication —
/// the data-plane counterpart to the `rcgen::RemoteKeyPair` impl above.
/// rustls only ever receives signatures from it; there is no code path that
/// can hand rustls a private key, because none exists outside the chip.
#[derive(Debug)]
pub struct TpmSigningKey(Arc<TpmSigner>);

impl rustls::sign::SigningKey for TpmSigningKey {
    /// The key is ECDSA P-256 with SHA-256 fixed at creation
    /// (`key_params()`), so exactly one scheme is ever possible. Returning
    /// `None` when the peer doesn't offer it makes rustls fall back to no
    /// client cert rather than negotiate something the TPM cannot sign.
    fn choose_scheme(
        &self,
        offered: &[rustls::SignatureScheme],
    ) -> Option<Box<dyn rustls::sign::Signer>> {
        offered
            .contains(&rustls::SignatureScheme::ECDSA_NISTP256_SHA256)
            .then(|| Box::new(TpmEcdsaSigner(self.0.clone())) as Box<dyn rustls::sign::Signer>)
    }

    fn algorithm(&self) -> rustls::SignatureAlgorithm {
        rustls::SignatureAlgorithm::ECDSA
    }

    // `public_key()` is deliberately left at its default `None`. It is an
    // optional hint used for raw-public-key mode (RFC 7250); our client
    // identity is the X.509 chain carried by the `CertifiedKey`, which is
    // what the connector/relay actually verifies.
}

#[derive(Debug)]
struct TpmEcdsaSigner(Arc<TpmSigner>);

impl rustls::sign::Signer for TpmEcdsaSigner {
    fn sign(&self, message: &[u8]) -> Result<Vec<u8>, rustls::Error> {
        self.0.sign_der(message).map_err(|e| {
            log_tpm_error("TLS client auth signing", &e);
            rustls::Error::General(format!("TPM signing failed: {e}"))
        })
    }

    fn scheme(&self) -> rustls::SignatureScheme {
        rustls::SignatureScheme::ECDSA_NISTP256_SHA256
    }
}

/// Both trait impls have to collapse a rich error into their own opaque
/// variant, so log the real cause first — otherwise a TPM that is locked
/// out, out of handles, or newly permission-denied surfaces as an
/// indistinguishable handshake failure.
fn log_tpm_error(context: &str, err: &anyhow::Error) {
    tracing::warn!(error = %err, "TPM {context} failed");
}

/// Builds the rustls signing key for a TPM-backed device. Paired with the
/// device's certificate chain in a `rustls::sign::CertifiedKey` by
/// `tunnel_pool::build_client_certified_key`.
pub fn signing_key(key_material: TpmKeyMaterial) -> Result<Arc<dyn rustls::sign::SigningKey>> {
    Ok(Arc::new(TpmSigningKey(Arc::new(new_signer(key_material)?))))
}

fn new_signer(key_material: TpmKeyMaterial) -> Result<TpmSigner> {
    let public_key_raw = encode_public_key(key_material.public())?;
    Ok(TpmSigner {
        ctx: Mutex::new(open_context()?),
        key_material,
        public_key_raw,
    })
}

/// DER-encodes an ECDSA-Sig-Value (`SEQUENCE { r INTEGER, s INTEGER }`,
/// RFC 3279 §2.2.3). The TPM hands back raw big-endian (r, s); X.509/CSR
/// signature fields need the DER SEQUENCE form. `IntegerAsn1::
/// from_bytes_be_unsigned` handles the DER-required leading-zero padding for
/// a high-bit-set magnitude, so a naive re-implementation isn't needed.
fn der_encode_ecdsa_signature(ecdsa: &EccSignature) -> Result<Vec<u8>> {
    use picky_asn1::wrapper::IntegerAsn1;
    use serde::Serialize;

    #[derive(Serialize)]
    struct EcdsaSigValue {
        r: IntegerAsn1,
        s: IntegerAsn1,
    }

    let sig = EcdsaSigValue {
        r: IntegerAsn1::from_bytes_be_unsigned(ecdsa.signature_r().value().to_vec()),
        s: IntegerAsn1::from_bytes_be_unsigned(ecdsa.signature_s().value().to_vec()),
    };
    picky_asn1_der::to_vec(&sig).map_err(|e| anyhow!("DER-encode ECDSA signature: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// PENDING-17 requirement D — the rustls `SigningKey`/`Signer` boundary,
    /// independent of any TLS handshake: rustls asks for a signature, the TPM
    /// produces it, and it verifies against the TPM's own public key under
    /// the exact algorithm rustls negotiated (ECDSA P-256 / SHA-256, DER).
    #[test]
    fn rustls_signer_signature_verifies_against_the_tpm_public_key() {
        if !tpm_available() {
            eprintln!("skipping: no accessible TPM on this machine");
            return;
        }

        let (key_material, public_key_raw) = generate().expect("generate TPM key");
        let signing_key = signing_key(key_material).expect("build rustls signing key");

        assert_eq!(signing_key.algorithm(), rustls::SignatureAlgorithm::ECDSA);

        // rustls offers the schemes the peer supports; ours is P-256/SHA-256.
        let signer = signing_key
            .choose_scheme(&[
                rustls::SignatureScheme::RSA_PKCS1_SHA256,
                rustls::SignatureScheme::ECDSA_NISTP256_SHA256,
            ])
            .expect("ECDSA_NISTP256_SHA256 was offered, so a signer is expected");
        assert_eq!(
            signer.scheme(),
            rustls::SignatureScheme::ECDSA_NISTP256_SHA256
        );

        // rustls hands the signer an UNHASHED message and expects the
        // signature format implied by the scheme — DER ECDSA-Sig-Value here.
        let message = b"rustls CertificateVerify transcript stand-in";
        let signature = signer.sign(message).expect("TPM signing");

        ring::signature::UnparsedPublicKey::new(
            &ring::signature::ECDSA_P256_SHA256_ASN1,
            &public_key_raw,
        )
        .verify(message, &signature)
        .expect("TPM signature must verify against the TPM public key");
    }

    /// A peer that cannot do P-256/SHA-256 must get `None` rather than a
    /// signer the TPM can't honour — rustls then proceeds without a client
    /// cert instead of failing mid-handshake on an unsignable scheme.
    #[test]
    fn rustls_signer_declines_schemes_the_tpm_key_cannot_produce() {
        if !tpm_available() {
            eprintln!("skipping: no accessible TPM on this machine");
            return;
        }

        let (key_material, _) = generate().expect("generate TPM key");
        let signing_key = signing_key(key_material).expect("build rustls signing key");

        assert!(signing_key
            .choose_scheme(&[
                rustls::SignatureScheme::RSA_PKCS1_SHA256,
                rustls::SignatureScheme::ED25519,
                rustls::SignatureScheme::ECDSA_NISTP384_SHA384,
            ])
            .is_none());
    }

    /// Real-hardware smoke test: generate a TPM key, reload it, sign a CSR
    /// with it, and verify the CSR's signature checks out — end to end,
    /// against whatever TPM this machine actually has. Skips (not fails)
    /// when no TPM is reachable, e.g. no /dev/tpmrm0 permission.
    #[test]
    fn tpm_generate_load_and_sign_a_real_csr() {
        if !tpm_available() {
            eprintln!("skipping: no accessible TPM on this machine");
            return;
        }

        let (key_material, public_key_raw) = generate().expect("generate TPM key");
        assert_eq!(public_key_raw[0], 0x04, "expected an uncompressed EC point");
        assert_eq!(public_key_raw.len(), 65, "P-256 uncompressed point is 65 bytes");

        let key_pair = load(key_material).expect("load TPM key");

        let mut params = rcgen::CertificateParams::default();
        params.distinguished_name = rcgen::DistinguishedName::new();
        params
            .distinguished_name
            .push(rcgen::DnType::CommonName, "tpm-smoke-test");
        let csr_pem = params
            .serialize_request(&key_pair)
            .expect("serialize CSR")
            .pem()
            .expect("PEM-encode CSR");

        use x509_parser::prelude::FromDer;

        let (_, pem) =
            x509_parser::pem::parse_x509_pem(csr_pem.as_bytes()).expect("decode CSR PEM");
        let (_, csr) = x509_parser::certification_request::X509CertificationRequest::from_der(
            &pem.contents,
        )
        .expect("parse CSR DER");
        csr.verify_signature().expect("CSR signature must verify against its own embedded public key");
    }
}
