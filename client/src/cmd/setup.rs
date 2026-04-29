use crate::config::{ClientConf, save};
use anyhow::Result;

pub async fn run(workspace: String, controller: Option<String>, connector: Option<String>) -> Result<()> {
    let conf = ClientConf {
        workspace,
        controller_address: controller.unwrap_or_default(),
        connector_address:  connector.unwrap_or_default(),
        ..Default::default()
    };
    let path = save(&conf)?;
    println!("Config written to {}", path.display());
    println!("Run `zecurity-client login` to authenticate.");
    Ok(())
}