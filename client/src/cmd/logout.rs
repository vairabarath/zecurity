use anyhow::Result;

pub async fn run() -> Result<()> {
    match crate::config::load() {
        Ok(conf) => {
            if crate::state_store::clear_workspace_state(&conf.workspace)? {
                println!("Logged out of {}.", conf.workspace);
            } else {
                println!("No saved session to clear.");
            }
        }
        Err(_) => println!("No saved session to clear."),
    }
    Ok(())
}
