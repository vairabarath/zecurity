use anyhow::Result;

use crate::{config::load, login};

pub async fn run() -> Result<()> {
    let conf = load()?;

    println!("Authenticating...");
    let result = login::run(&conf, None).await?;

    println!("Logged in as {}", result.user.email);
    println!("Workspace: {}", result.workspace.name);
    println!("Device ID: {}", result.device.id);
    Ok(())
}
