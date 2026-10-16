use std::path::{Path, PathBuf};

fn main() -> Result<(), Box<dyn std::error::Error>> {
    tonic_build::configure()
        // Server codegen is unused in production (this binary only ever
        // dials OUT to the Go controller as a client) but is enabled so
        // daemon.rs's e2e tests can stand up a real in-process
        // ClientServiceServer to drive RenewCert against — see
        // daemon::renewal_tests. Dead weight in the release binary, never
        // instantiated by main.rs.
        .build_server(true)
        .build_client(true)
        .compile_protos(&["../proto/client/v1/client.proto"], &["../proto"])?;
    embed_platform_root()?;
    Ok(())
}

/// Certificate audit bug 1 (client): the client's trust anchor is the
/// platform root CA compiled into the binary, never anything downloaded.
/// `ZECURITY_PLATFORM_ROOT_PEM_FILE` names the root's PEM (a path relative
/// to this crate, or absolute). It must hold exactly one self-signed CA
/// certificate. A release build without it fails; a debug build without it
/// compiles, but login refuses to run.
fn embed_platform_root() -> Result<(), Box<dyn std::error::Error>> {
    const VAR: &str = "ZECURITY_PLATFORM_ROOT_PEM_FILE";
    println!("cargo:rerun-if-env-changed={VAR}");
    let out = PathBuf::from(std::env::var("OUT_DIR")?).join("platform_root.rs");

    let path = match std::env::var(VAR) {
        Ok(p) if !p.trim().is_empty() => {
            let p = PathBuf::from(p.trim());
            if p.is_absolute() {
                p
            } else {
                Path::new(&std::env::var("CARGO_MANIFEST_DIR")?).join(p)
            }
        }
        _ => {
            if std::env::var("PROFILE").as_deref() == Ok("release") {
                return Err(format!(
                    "{VAR} is not set: a release client must embed the platform root CA \
                     (certificate audit bug 1). Official releases use \
                     client/trust/platform-root.pem; for a dev or LAN controller, build \
                     with client/scripts/dev-build.sh."
                )
                .into());
            }
            println!(
                "cargo:warning={VAR} not set: this debug build has no platform root and \
                 `zecurity-client login` will refuse to run (see client/scripts/dev-build.sh)"
            );
            std::fs::write(
                &out,
                "pub const EMBEDDED_PLATFORM_ROOT_PEM: Option<&str> = None;\n",
            )?;
            return Ok(());
        }
    };
    println!("cargo:rerun-if-changed={}", path.display());

    let pem = std::fs::read_to_string(&path)
        .map_err(|e| format!("{VAR}: cannot read {}: {e}", path.display()))?;
    check_root(&pem).map_err(|e| format!("{VAR}: {}: {e}", path.display()))?;
    std::fs::write(
        &out,
        format!("pub const EMBEDDED_PLATFORM_ROOT_PEM: Option<&str> = Some({pem:?});\n"),
    )?;
    Ok(())
}

/// Exactly one certificate, a CA, self-issued and self-signed.
fn check_root(pem: &str) -> Result<(), String> {
    use x509_parser::prelude::*;
    let blocks = x509_parser::pem::Pem::iter_from_buffer(pem.as_bytes())
        .collect::<Result<Vec<_>, _>>()
        .map_err(|e| format!("not PEM: {e:?}"))?;
    let [block] = blocks.as_slice() else {
        return Err(format!(
            "expected exactly one certificate, found {}",
            blocks.len()
        ));
    };
    let (_, cert) = X509Certificate::from_der(&block.contents)
        .map_err(|e| format!("not a certificate: {e:?}"))?;
    if !cert.is_ca() {
        return Err("not a CA certificate".into());
    }
    if cert.issuer() != cert.subject() {
        return Err("not a root: issuer differs from subject".into());
    }
    cert.verify_signature(None)
        .map_err(|e| format!("not self-signed: {e:?}"))?;
    Ok(())
}
