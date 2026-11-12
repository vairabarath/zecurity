use anyhow::Result;

use crate::{
    config::load,
    ipc::{ensure_daemon_and_send, IpcRequest},
    login,
};

pub async fn run() -> Result<()> {
    let conf = load()?;

    // F-4 (live run 2026-10-29): a device the admin revoked leaves a state
    // that makes every daemon start exit again; signing in registers a new
    // device, so the dead one's state goes first.
    if crate::state_store::clear_revoked_state(&conf.workspace)? {
        println!("This device was revoked; signing in registers it again as a new device.");
    }
    // Reach the daemon before enrolling: enrolling first left an active
    // device on the server, never saved here, on every failed attempt.
    if ensure_daemon_and_send(&IpcRequest::Status).await.is_err() {
        anyhow::bail!(
            "the Zecurity client service is not running — start it with \
             `sudo systemctl start zecurity-client`, then run `zecurity-client login` again"
        );
    }

    // PENDING-09: the login this one replaces, if any (clear_revoked_state has
    // already removed a revoked one).
    let previous = crate::state_store::load_workspace_state(&conf.workspace).ok();

    println!("Authenticating...");
    let result = login::run(&conf, None).await?; /* the values are = 
        workspace: WorkspaceInfo {
            id: String::new(),
            name: conf.workspace.clone(),
            slug: conf.workspace.clone(),
            trust_domain: extract_trust_domain(&enroll.spiffe_id),
        },
        user: UserInfo {
            id: String::new(),
            email: tok.email.clone(),
            role: String::new(),
        },
        device: DeviceInfo {
            id: enroll.device_id,
            spiffe_id: enroll.spiffe_id,
            certificate_pem: enroll.certificate_pem,
            private_key_pem,
            ca_cert_pem,
            cert_expires_at,
            hostname,
            os,
        },
        session: SessionInfo {
            access_token: tok.access_token,
            refresh_token: tok.refresh_token,
            expires_at: now + tok.expires_in,
        }, 
        
        -> actual values <-
        ###################
        LoginResult {
            workspace: WorkspaceInfo {
                id: "",
                name: "acme",
                slug: "acme",
                trust_domain: "acme.zecurity.io",
            },

            user: UserInfo {
                id: "",
                email: "alice@company.com",
                role: "",
            },

            device: DeviceInfo {
                id: "dev-001",
                spiffe_id: "spiffe://acme.zecurity.io/client/dev-001",

                certificate_pem: "...",
                private_key_pem: "...",
                ca_cert_pem: "...",

                cert_expires_at: 1784000000,

                hostname: "ubuntu-server",
                os: "linux",
            },

            session: SessionInfo {
                access_token: "jwt-access-token",
                refresh_token: "jwt-refresh-token",
                expires_at: 1780000000,
            },
        }
        */

    let email = result.user.email.clone();
    let workspace = result.workspace.name.clone();
    let device_id = result.device.id.clone();

    let resp = ensure_daemon_and_send(&IpcRequest::PostLoginState {
        workspace_slug: conf.workspace.clone(),
        workspace_name: result.workspace.name,
        workspace_id: result.workspace.id,
        trust_domain: result.workspace.trust_domain,
        user_email: result.user.email,
        access_token: result.session.access_token,
        refresh_token: result.session.refresh_token,
        expires_at: result.session.expires_at,
        device_id: result.device.id,
        spiffe_id: result.device.spiffe_id,
        certificate_pem: result.device.certificate_pem,
        private_key_pem: result.device.private_key_pem,
        tpm_key_material: result.device.tpm_key_material,
        ca_cert_pem: result.device.ca_cert_pem,
        cert_expires_at: result.device.cert_expires_at,
        hostname: result.device.hostname,
        os: result.device.os,
    })
    .await?;
    if !resp.ok {
        anyhow::bail!("{}", resp.error.unwrap_or_else(|| "unknown error".into()));
    }

    // PENDING-09: a different user signing in on this client ends the
    // previous user's session, which revokes the device enrolled under it
    // (the controller does this even if that session's token has expired).
    // After PostLoginState: the daemon has switched to the new device, so the
    // old device's revocation cannot shut it down mid-switch.
    // The same user signing in again keeps today's behaviour: the previous
    // device ends when its own session does.
    if let Some(prev) = previous.as_ref() {
        if is_different_user(&prev.user.email, &email) && !prev.session.access_token.is_empty() {
            let ended = match crate::trust::embedded_platform_root_pem() {
                Ok(root) => {
                    crate::cmd::logout::end_session(
                        &conf,
                        &prev.session.access_token,
                        &prev.device.ca_cert_pem,
                        root,
                    )
                    .await
                }
                Err(e) => Err(e),
            };
            match ended {
                Ok(()) => println!(
                    "Signed out the previous user ({}) on this device.",
                    prev.user.email
                ),
                Err(e) => eprintln!(
                    "warning: could not sign out the previous user ({}): {e:#}",
                    prev.user.email
                ),
            }
        }
    }

    println!("Logged in as {}", email);
    println!("Workspace: {}", workspace);
    println!("Device ID: {}", device_id);
    // A successful PostLoginState has already brought the tunnel up (or
    // restarted it onto the new state); a second Up would only fail with
    // "already up" and turn a good login into a non-zero exit.
    println!("Zecurity is up.");
    Ok(())
}

/// True when the login replacing a saved one belongs to another user. Emails
/// compare case-insensitively; an empty saved email (no previous login)
/// counts as the same user, so nothing is ended.
fn is_different_user(previous_email: &str, new_email: &str) -> bool {
    let prev = previous_email.trim();
    !prev.is_empty() && !prev.eq_ignore_ascii_case(new_email.trim())
}

#[cfg(test)]
mod different_user_tests {
    use super::is_different_user;

    #[test]
    fn only_another_user_counts_as_different() {
        assert!(is_different_user("alice@example.com", "bob@example.com"));
        assert!(!is_different_user("alice@example.com", "alice@example.com"));
        assert!(!is_different_user(
            "Alice@Example.com",
            " alice@example.com "
        ));
        assert!(!is_different_user("", "bob@example.com"));
    }
}
