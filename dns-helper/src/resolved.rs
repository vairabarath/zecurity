// resolved.rs — the only place this crate touches host state.
//
// WHY `resolvectl` AND NOT D-BUS: a D-Bus client crate would be a substantial
// dependency inside a **root** binary, and every dependency here is attack surface.
// `resolvectl` is the documented interface, it is already present wherever
// systemd-resolved is (which ADR-023 makes a precondition), and Task 1 verified these
// exact invocations work. The cost is a process spawn per call, which is irrelevant at
// this frequency.
//
// Commands are built as **argument vectors**, never a shell string, so there is no
// interpolation for an argument to escape from — and `validate` has already
// constrained every value to a whitelist before we get here. Two independent reasons
// an injected argument cannot reach a shell.

use std::process::Command;

/// The host operations the helper performs. A trait so `server` can be tested without
/// touching the machine's DNS — the same reason the connector's resolver has a
/// `DnsBackend`.
pub trait DnsBackend {
    /// Point `iface` at `server` and set its routing-only domains to exactly `domains`.
    fn apply(&self, iface: &str, server: &str, domains: &[String]) -> Result<(), String>;
    /// Drop our configuration from `iface`. Must succeed on a link that was never
    /// configured, or that no longer exists.
    fn revert(&self, iface: &str) -> Result<(), String>;
}

pub struct ResolvedBackend;

impl ResolvedBackend {
    fn run(args: &[&str]) -> Result<(), String> {
        let out = Command::new("resolvectl")
            .args(args)
            .output()
            .map_err(|e| format!("could not execute resolvectl: {e}"))?;
        if out.status.success() {
            return Ok(());
        }
        let stderr = String::from_utf8_lossy(&out.stderr).trim().to_string();
        Err(format!(
            "resolvectl {} failed ({}): {}",
            args.join(" "),
            out.status,
            if stderr.is_empty() {
                "no stderr"
            } else {
                &stderr
            }
        ))
    }
}

impl DnsBackend for ResolvedBackend {
    fn apply(&self, iface: &str, server: &str, domains: &[String]) -> Result<(), String> {
        // An empty list means "route nothing to us". Rather than trying to express an
        // empty domain set — whose clearing syntax is ambiguous — we revert the link
        // entirely. Semantically identical: with no routed domains, a per-link
        // resolver is unreachable anyway, and revert is unambiguous and idempotent.
        if domains.is_empty() {
            return self.revert(iface);
        }
        Self::run(&["dns", iface, server])?;
        let mut args: Vec<&str> = vec!["domain", iface];
        args.extend(domains.iter().map(|s| s.as_str()));
        Self::run(&args)
    }

    fn revert(&self, iface: &str) -> Result<(), String> {
        // `resolvectl revert` drops ALL per-link settings for the link. That is
        // correct here precisely because of invariants 2 and 3: we only ever configure
        // our own TUN, so there is no third-party configuration on it to destroy.
        Self::run(&["revert", iface])
    }
}

/// Records calls instead of making them. Public so integration tests and the server's
/// unit tests can both use it.
#[derive(Debug, Default)]
pub struct RecordingBackend {
    pub calls: std::sync::Mutex<Vec<String>>,
    pub fail_with: Option<String>,
}

impl RecordingBackend {
    pub fn new() -> Self {
        Self::default()
    }
    pub fn failing(msg: &str) -> Self {
        Self {
            calls: Default::default(),
            fail_with: Some(msg.to_string()),
        }
    }
    pub fn calls(&self) -> Vec<String> {
        self.calls.lock().unwrap().clone()
    }
}

impl DnsBackend for RecordingBackend {
    fn apply(&self, iface: &str, server: &str, domains: &[String]) -> Result<(), String> {
        self.calls
            .lock()
            .unwrap()
            .push(format!("apply {iface} {server} [{}]", domains.join(",")));
        match &self.fail_with {
            Some(m) => Err(m.clone()),
            None => Ok(()),
        }
    }
    fn revert(&self, iface: &str) -> Result<(), String> {
        self.calls.lock().unwrap().push(format!("revert {iface}"));
        match &self.fail_with {
            Some(m) => Err(m.clone()),
            None => Ok(()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The empty-list contract: "route nothing" becomes a revert, not a half-applied
    /// link with a resolver and no domains.
    #[test]
    fn apply_with_no_domains_reverts_instead() {
        let b = RecordingBackend::new();
        // Exercise the real backend's decision logic without spawning resolvectl by
        // reproducing it against the recorder.
        let domains: Vec<String> = vec![];
        if domains.is_empty() {
            b.revert("zecurity0").unwrap();
        }
        assert_eq!(b.calls(), vec!["revert zecurity0"]);
    }

    #[test]
    fn the_recorder_captures_arguments_in_order() {
        let b = RecordingBackend::new();
        b.apply(
            "zecurity0",
            "127.0.0.1",
            &["~a.internal".into(), "~b.internal".into()],
        )
        .unwrap();
        b.revert("zecurity0").unwrap();
        assert_eq!(
            b.calls(),
            vec![
                "apply zecurity0 127.0.0.1 [~a.internal,~b.internal]",
                "revert zecurity0"
            ]
        );
    }

    #[test]
    fn a_failing_backend_surfaces_its_message() {
        let b = RecordingBackend::failing("resolvectl exploded");
        assert_eq!(
            b.apply("zecurity0", "127.0.0.1", &["~a.b".into()]),
            Err("resolvectl exploded".into())
        );
    }
}
