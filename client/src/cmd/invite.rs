use anyhow::{anyhow, Result};
use reqwest::Client;
use serde::{Deserialize, Serialize};

use crate::config::load;
use crate::ipc::{send_ipc, IpcRequest};
use crate::login;

#[derive(Serialize)]
struct InviteRequest<'a> {
    email: &'a str,
}

#[derive(Deserialize)]
struct InviteResponse {
    email: String,
    expires_at: String,
}

pub async fn run(email: String) -> Result<()> {
    let conf = load()?;

    // Use daemon token if available — avoids forcing re-auth on every invite.
    let access_token = match send_ipc(&IpcRequest::GetToken).await {
        Ok(resp) if resp.ok => resp.token.unwrap_or_default(),
        _ => {
            println!("Authenticating...");
            login::run(&conf, None).await?.session.access_token
        }
    };

    let url = format!("{}/api/invitations", conf.http_base());

    let resp = Client::new()
        .post(&url)
        .bearer_auth(&access_token)
        .json(&InviteRequest { email: &email })
        .send()
        .await?;

    match resp.status().as_u16() {
        201 => {
            let inv: InviteResponse = resp.json().await?;
            println!(
                "Invitation sent to {} (expires: {})",
                inv.email, inv.expires_at
            );
        }
        401 => {
            return Err(anyhow!(
                "Session expired. Run `zecurity-client login` again."
            ))
        }
        403 => return Err(anyhow!("Permission denied. Only admins can invite users.")),
        409 => {
            return Err(anyhow!(
                "{} is already invited or a workspace member.",
                email
            ))
        }
        s => {
            let body = resp.text().await.unwrap_or_default();
            return Err(anyhow!("Error {}: {}", s, body));
        }
    }
    Ok(())
}
