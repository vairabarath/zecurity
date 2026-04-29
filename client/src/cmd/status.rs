use anyhow::Result;

pub async fn run() -> Result<()> {
    match crate::config::load() {
        Ok(conf) => {
            println!("Workspace:  {}", conf.workspace);
            println!("Controller: {}", conf.controller());
            println!("Status:     Not connected (run login to authenticate)");
        }
        Err(_) => {
            println!("Status:    Not configured");
            println!("Run `zecurity-client setup --workspace <name>` first.");
        }
    }
    Ok(())
}