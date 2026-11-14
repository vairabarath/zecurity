//! Drops log lines that third-party crates report as errors although they are
//! part of normal operation, so a real error in the Windows daemon log is not
//! lost among them. Each entry names one crate target and one message; any
//! other message from the same crate is still logged.
//!
//! - `wintun::log`: "Failed to find matching adapter name … (Code 0x00000490)".
//!   Before creating `zecurity0`, Wintun looks for an existing adapter of that
//!   name to reuse; when there is none (every fresh start) it logs this as an
//!   error and then creates the adapter.
//! - `tun::platform::windows::device`: `receive_blocking error: … shuting down`
//!   (sic, Wintun's spelling). Tunnel-down ends the Wintun session under the
//!   packet reader thread, which reports the shutdown as an error.

use std::fmt;

use tracing::field::{Field, Visit};
use tracing::{Event, Subscriber};
use tracing_subscriber::layer::{Context, Layer};

/// True for a known-harmless message from `target`.
pub(crate) fn is_benign(target: &str, message: &str) -> bool {
    match target {
        "wintun::log" => {
            message.starts_with("WinTun: Failed to find matching adapter name")
                && message.contains("0x00000490")
        }
        "tun::platform::windows::device" => {
            message.starts_with("receive_blocking error") && message.contains("shuting down")
        }
        _ => false,
    }
}

/// Layer that disables the events `is_benign` matches. These crates log
/// through the `log` crate, so the target and message arrive as the
/// `log.target` and `message` fields of the bridged event.
pub(crate) struct DropBenignNoise;

#[derive(Default)]
struct Fields {
    target: Option<String>,
    message: Option<String>,
}

impl Visit for Fields {
    fn record_str(&mut self, field: &Field, value: &str) {
        match field.name() {
            "log.target" => self.target = Some(value.to_owned()),
            "message" => self.message = Some(value.to_owned()),
            _ => {}
        }
    }

    fn record_debug(&mut self, field: &Field, value: &dyn fmt::Debug) {
        match field.name() {
            "log.target" => self.target = Some(format!("{value:?}")),
            "message" => self.message = Some(format!("{value:?}")),
            _ => {}
        }
    }
}

impl<S: Subscriber> Layer<S> for DropBenignNoise {
    fn event_enabled(&self, event: &Event<'_>, _ctx: Context<'_, S>) -> bool {
        // Only the two crates' events are inspected; everything else passes
        // without visiting its fields.
        let meta_target = event.metadata().target();
        if meta_target != "log"
            && !meta_target.starts_with("wintun")
            && !meta_target.starts_with("tun::")
        {
            return true;
        }
        let mut f = Fields::default();
        event.record(&mut f);
        let target = f.target.as_deref().unwrap_or(meta_target);
        match f.message.as_deref() {
            Some(m) => !is_benign(target, m),
            None => true,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{Arc, Mutex};
    use tracing_subscriber::layer::SubscriberExt;

    #[test]
    fn matches_only_the_known_messages() {
        assert!(is_benign(
            "wintun::log",
            "WinTun: Failed to find matching adapter name: Element not found. (Code 0x00000490)"
        ));
        assert!(is_benign(
            "tun::platform::windows::device",
            "receive_blocking error: String(\"Session UnsafeHandle(0x1e6a4070b20) shuting down\")"
        ));
        // Same crates, other errors: kept.
        assert!(!is_benign(
            "wintun::log",
            "WinTun: Failed to create adapter: Access is denied. (Code 0x00000005)"
        ));
        assert!(!is_benign(
            "wintun::log",
            "WinTun: Failed to find matching adapter name: (Code 0x00000005)"
        ));
        assert!(!is_benign(
            "tun::platform::windows::device",
            "receive_blocking error: Os { code: 6 }"
        ));
        // Same text from another target: kept.
        assert!(!is_benign(
            "zecurity_core::daemon",
            "receive_blocking error: shuting down"
        ));
    }

    /// Collects the messages that reach the layer after the filter.
    struct Capture(Arc<Mutex<Vec<String>>>);
    impl<S: Subscriber> Layer<S> for Capture {
        fn on_event(&self, event: &Event<'_>, _ctx: Context<'_, S>) {
            let mut f = Fields::default();
            event.record(&mut f);
            self.0.lock().unwrap().push(f.message.unwrap_or_default());
        }
    }

    #[test]
    fn layer_drops_benign_events_and_keeps_others() {
        let seen = Arc::new(Mutex::new(Vec::new()));
        let sub = tracing_subscriber::registry()
            .with(DropBenignNoise)
            .with(Capture(seen.clone()));
        tracing::subscriber::with_default(sub, || {
            tracing::error!(
                target: "tun::platform::windows::device",
                "receive_blocking error: {:?}",
                "Session UnsafeHandle(0x1) shuting down"
            );
            tracing::error!(target: "tun::platform::windows::device", "receive_blocking error: Os {{ code: 6 }}");
            tracing::error!(target: "zecurity_core::daemon", "tunnel failed");
        });
        let seen = seen.lock().unwrap();
        assert_eq!(seen.len(), 2, "{seen:?}");
        assert!(seen[0].contains("code: 6"));
        assert_eq!(seen[1], "tunnel failed");
    }
}
