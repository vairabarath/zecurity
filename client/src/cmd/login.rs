use anyhow::Result;

use crate::{
    config::load,
    login,
    state_store::{save_workspace_state, StoredWorkspaceState},
};

pub async fn run() -> Result<()> {
    let conf = load()?;

    println!("Authenticating...");
    let result = login::run(&conf, None).await?;
    let state = StoredWorkspaceState::from_login(result);
    save_workspace_state(&conf.workspace, &state)?;

    println!("Logged in as {}", state.user.email);
    println!("Workspace: {}", state.workspace.name);
    println!("Device ID: {}", state.device.id);
    Ok(())
}
