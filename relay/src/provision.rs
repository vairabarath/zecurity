use anyhow::Result;
use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::Path;

use crate::csr::generate_relay_csr;

pub fn write_relay_material(relay_id: &str) -> Result<()> {
    let material = generate_relay_csr(relay_id)?;

    let out_dir = Path::new("pki");

    fs::create_dir_all(out_dir)?;
    let key_path = out_dir.join("relay.key");
    let csr_path = out_dir.join("relay.csr");
    fs::write(&key_path, material.private_key_pem)?;
    let mut perms = fs::metadata(&key_path)?.permissions();

    perms.set_mode(0o600);

    fs::set_permissions(&key_path, perms)?;

    let mut perms = fs::metadata(&key_path)?.permissions();

    perms.set_mode(0o600);

    fs::set_permissions(&key_path, perms)?;
    fs::write(&csr_path, material.csr_pem)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn creates_material() {
        let relay_id = "550e8400-e29b-41d4-a716-446655440000";

        write_relay_material(relay_id).unwrap();

        assert!(std::path::Path::new("pki/relay.key").exists());

        assert!(std::path::Path::new("pki/relay.csr").exists());
    }
}
