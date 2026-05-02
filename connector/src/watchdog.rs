use std::env;
use std::time::Duration;

use tokio::time::interval;

fn sd_notify(msg: &str) {
    let Ok(sock_path) = env::var("NOTIFY_SOCKET") else { return };
    let _ = std::os::unix::net::UnixDatagram::unbound()
        .and_then(|s| s.send_to(msg.as_bytes(), &sock_path));
}

/// Call once after all listeners are bound and ready to serve traffic.
/// Sends `READY=1` to systemd via the sd_notify socket.
/// Safe to call when not running under systemd — no-op if NOTIFY_SOCKET is unset.
pub fn notify_ready() {
    sd_notify("READY=1\n");
}

/// Spawns a background task that sends `WATCHDOG=1` at half the WatchdogUSec interval.
/// Safe to call even when WATCHDOG_USEC is not set — no-op if unset or unparseable.
pub fn spawn_watchdog() {
    let Some(usec_str) = env::var("WATCHDOG_USEC").ok() else { return };
    let Ok(usec) = usec_str.parse::<u64>() else { return };
    let interval_ms = usec / 2 / 1000;
    tokio::spawn(async move {
        let mut tick = interval(Duration::from_millis(interval_ms));
        loop {
            tick.tick().await;
            sd_notify("WATCHDOG=1\n");
        }
    });
}
