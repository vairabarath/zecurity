use anyhow::Result;

pub async fn run() -> Result<()> {
    match crate::config::load() {
        Ok(conf) => {
            println!("Workspace:  {}", conf.workspace);
            println!("Controller: {}", conf.controller());
            match crate::state_store::load_workspace_state(&conf.workspace) {
                Ok(state) => {
                    println!(
                        "Status:     Logged in as {}, cert expires in {}",
                        state.user.email,
                        crate::state_store::format_duration_until(state.device.cert_expires_at)
                    );
                    println!("Device ID:  {}", state.device.id);
                    println!("SPIFFE ID:  {}", state.device.spiffe_id);
                }
                Err(_) => println!("Status:     Not connected (run login to authenticate)"),
            }
        }
        Err(_) => {
            println!("Status:    Not configured");
            println!("Run `zecurity-client setup --workspace <name>` first.");
        }
    }
    Ok(())
}
