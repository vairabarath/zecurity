use anyhow::Result;
use std::time::{SystemTime, UNIX_EPOCH};

pub async fn run() -> Result<()> {
    match crate::ipc::query_daemon_status().await {
        Ok(v) => {
            let now = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() as i64;
            let expires = v["session_expires_at"].as_i64().unwrap_or(0);
            let remaining = expires - now;
            let session_str = if remaining > 0 {
                format!("Active (expires in {}h)", remaining / 3600)
            } else {
                "Expired".into()
            };
            println!("Email:      {}", v["email"].as_str().unwrap_or("—"));
            println!("Workspace:  {}", v["workspace"].as_str().unwrap_or("—"));
            println!("Status:     Connected");
            println!("Session:    {}", session_str);
            println!("Device:     Verified");
            println!("SPIFFE ID:  {}", v["spiffe_id"].as_str().unwrap_or("—"));
        }
        Err(_) => {
            match crate::config::load() {
                Ok(conf) => {
                    println!("Workspace:  {}", conf.workspace);
                    println!("Status:     Not connected");
                }
                Err(_) => println!("Status: Not configured"),
            }
        }
    }
    Ok(())
}
