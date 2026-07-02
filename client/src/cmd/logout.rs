use anyhow::Result;

use crate::ipc::{send_ipc, IpcRequest};

pub async fn run() -> Result<()> {
    let Ok(conf) = crate::config::load() else {
        println!("No saved session to clear.");
        return Ok(());
    };

    // Read the current access token (if any) before we touch the daemon or
    // the state file. On failure or missing state, skip the server call —
    // we still want the local logout to succeed.
    let access_token = crate::state_store::load_workspace_state(&conf.workspace)
        .ok()
        .map(|s| s.session.access_token)
        .filter(|t| !t.is_empty());

    // Best-effort server-side revoke — invalidates the refresh token in
    // Redis so a leaked copy cannot be replayed. Network / server failures
    // are logged and swallowed; local logout MUST proceed even if the
    // controller is unreachable.
    if let Some(token) = access_token {
        if let Err(e) = crate::auth::logout(&conf, &token).await {
            eprintln!("warning: server-side logout failed: {e:#} (local session will still be cleared)");
        }
    }

    // Best-effort shutdown — drop runtime state from daemon memory.
    let _ = send_ipc(&IpcRequest::Shutdown).await;

    if crate::state_store::clear_workspace_state(&conf.workspace)? {
        println!("Logged out of {}.", conf.workspace);
    } else {
        println!("No saved session to clear.");
    }
    Ok(())
}
