use anyhow::Result;

pub async fn run() -> Result<()> {
    println!("No active session to clear.");
    println!("Session is in-memory only — process exit clears everything.");
    Ok(())
}