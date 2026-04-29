use anyhow::Result;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use tokio::signal;
use tokio::time::{sleep, Duration};

use crate::{config::load, ipc::serve_ipc, login, runtime::new_shared};

pub async fn run() -> Result<()> {
    let conf = load()?;
    let state = new_shared();
    let shutdown = Arc::new(AtomicBool::new(false));

    tokio::spawn(serve_ipc(state.clone()));

    sd_notify("READY=1");

    loop {
        if shutdown.load(Ordering::Relaxed) {
            println!("Shutting down.");
            break;
        }

        println!("Authenticating...");
        match login::run(&conf, None).await {
            Ok(result) => {
                {
                    let mut st = state.write().await;
                    st.workspace = Some(result.workspace);
                    st.user      = Some(result.user);
                    st.device    = Some(result.device);
                    st.session   = Some(result.session);
                }
                let email = state.read().await.user.as_ref().map(|u| u.email.clone()).unwrap_or_default();
                println!("Connected as {}", email);

                // Phase 5 replaces tunnel_placeholder() with TunTunnel::run()
                tokio::select! {
                    _ = tunnel_placeholder() => {
                        eprintln!("Tunnel ended. Reconnecting in 5s...");
                    }
                    _ = signal::ctrl_c() => {
                        println!("Shutting down.");
                        return Ok(());
                    }
                }
            }
            Err(e) => {
                eprintln!("Login failed: {}. Retrying in 10s...", e);
                tokio::select! {
                    _ = sleep(Duration::from_secs(10)) => {}
                    _ = signal::ctrl_c() => return Ok(()),
                }
                continue;
            }
        }

        *state.write().await = Default::default();

        tokio::select! {
            _ = sleep(Duration::from_secs(5)) => {}
            _ = signal::ctrl_c() => return Ok(()),
        }

        sd_notify("WATCHDOG=1");
    }
    Ok(())
}

async fn tunnel_placeholder() {
    sleep(Duration::from_secs(u64::MAX)).await;
}

fn sd_notify(msg: &str) {
    if let Ok(addr) = std::env::var("NOTIFY_SOCKET") {
        use std::os::unix::net::UnixDatagram;
        if let Ok(sock) = UnixDatagram::unbound() {
            sock.send_to(msg.as_bytes(), addr.trim_start_matches('@')).ok();
        }
    }
}
