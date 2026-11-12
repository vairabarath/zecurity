//! PENDING-14 F3: systemd-resolved wiring for DNS-name resources.
//!
//! When the applied ACL holds DNS-name resources, `zecurity0` gets:
//! - a main-table route to the DNS answerer, `198.19.255.53/32 dev zecurity0`
//!   (resolved sends its queries on the link; smoltcp answers them, `dns.rs`);
//! - per-link DNS `198.19.255.53`, one `~<name>` routing domain per name, and
//!   `default-route false`, so only these names are sent to the client and
//!   all other DNS is untouched.
//!
//! The daemon runs as the installing user with `CAP_NET_ADMIN` only; resolved
//! allows these calls through the polkit rule the installer adds (F-8,
//! lab-verified 2026-11-03). Without resolved, or if a call fails, name
//! resources stay inactive and `zecurity-client status` says why; IP resources
//! are unaffected. The client never edits `/etc/hosts` or `resolv.conf`.

use std::collections::BTreeSet;
use std::sync::Mutex;

use crate::name_map::{self, DNS_RESPONDER_ADDR};
use crate::runtime::AppliedConfig;
use crate::tun::CmdRunner;

const LINK: &str = "zecurity0";

/// Seconds each resolved/ip call may take (coreutils `timeout`).
const COMMAND_TIMEOUT: &str = "5";

/// The DNS-name resources of an applied config: TCP (or empty protocol)
/// entries whose address is a resource name.
pub(crate) fn names_in(cfg: &AppliedConfig) -> BTreeSet<String> {
    cfg.entries
        .iter()
        .filter(|e| e.protocol.is_empty() || e.protocol.eq_ignore_ascii_case("tcp"))
        .filter(|e| name_map::is_resource_name(&e.address))
        .map(|e| e.address.clone())
        .collect()
}

/// The commands that make resolved send exactly `names` to the client.
pub(crate) fn desired_commands(names: &BTreeSet<String>) -> Vec<(&'static str, Vec<String>)> {
    let dns = DNS_RESPONDER_ADDR.to_string();
    if names.is_empty() {
        return vec![("resolvectl", vec!["revert".into(), LINK.into()])];
    }
    let mut domain = vec!["domain".to_string(), LINK.to_string()];
    domain.extend(names.iter().map(|n| format!("~{n}")));
    vec![
        (
            "ip",
            vec![
                "route".into(),
                "replace".into(),
                format!("{dns}/32"),
                "dev".into(),
                LINK.into(),
            ],
        ),
        ("resolvectl", vec!["dns".into(), LINK.into(), dns]),
        ("resolvectl", domain),
        (
            "resolvectl",
            vec!["default-route".into(), LINK.into(), "false".into()],
        ),
    ]
}

/// What `zecurity-client status` reports about DNS-name resources.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub(crate) struct DnsState {
    /// The names resolved has been configured for (what is live).
    pub configured: BTreeSet<String>,
    /// None: no DNS-name resources. Some(Ok(n)): n names active.
    /// Some(Err(reason)): names present but not resolvable.
    pub status: Option<Result<usize, String>>,
}

static STATE: Mutex<Option<DnsState>> = Mutex::new(None);

fn state() -> DnsState {
    STATE
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .clone()
        .unwrap_or_default()
}

fn set_state(s: DnsState) {
    *STATE.lock().unwrap_or_else(|e| e.into_inner()) = Some(s);
}

/// One status line, or None when there are no DNS-name resources.
pub(crate) fn status_line() -> Option<String> {
    match state().status? {
        Ok(n) => Some(format!(
            "active ({n} name{})",
            if n == 1 { "" } else { "s" }
        )),
        Err(reason) => Some(format!("unavailable: {reason}")),
    }
}

/// Brings resolved and the active name set in line with `cfg`. `force`
/// re-applies even when the names didn't change (a new TUN link lost its
/// resolved config). Returns the new state; never fails the caller: a DNS
/// problem must not take IP resources down.
#[cfg_attr(any(windows, target_os = "android", target_os = "ios"), allow(unreachable_code, unused_variables))]
pub(crate) fn sync(runner: &dyn CmdRunner, cfg: &AppliedConfig, force: bool) -> DnsState {
    let names = names_in(cfg);
    // Windows has no systemd-resolved; its equivalent (NRPT rules for the
    // names) is not built yet. IP resources are unaffected, exactly as on a
    // Linux host without resolved.
    #[cfg(any(windows, target_os = "android", target_os = "ios"))]
    return unsupported(names);
    let prev = state();
    if !force && prev.configured == names && prev.status.is_some() != names.is_empty() {
        name_map::with(|m| m.set_active(names.clone()));
        return prev;
    }

    if let Some(ip) = name_map::with(|m| m.conflict()) {
        name_map::with(|m| m.set_active(BTreeSet::new()));
        let s = DnsState {
            configured: BTreeSet::new(),
            status: (!names.is_empty())
                .then(|| Err(format!("resource address {ip} is inside 198.18.0.0/15, reserved for DNS name resources; change or remove it, then restart the tunnel (zecurity-client down && zecurity-client up)"))),
        };
        set_state(s.clone());
        return s;
    }

    let mut result = Ok(names.len());
    for (program, args) in desired_commands(&names) {
        // Bounded: a stuck resolved or D-Bus must not stall bring-up or a
        // hot-apply (the daemon's watchdog would restart it).
        let mut argv: Vec<&str> = vec![COMMAND_TIMEOUT, program];
        argv.extend(args.iter().map(String::as_str));
        if let Err(e) = runner.run("timeout", &argv, None) {
            if names.is_empty() {
                // Nothing to resolve; a failed revert (no resolved, link
                // already gone) changes nothing.
                tracing::debug!(error = %e, "resolvectl revert failed");
            } else {
                tracing::warn!(error = %e, "DNS name resources unavailable: systemd-resolved configuration failed");
                result = Err(format!("systemd-resolved configuration failed: {e}"));
            }
            break;
        }
    }
    let ok = result.is_ok();
    name_map::with(|m| m.set_active(if ok { names.clone() } else { BTreeSet::new() }));
    let s = DnsState {
        configured: if ok { names.clone() } else { BTreeSet::new() },
        status: (!names.is_empty()).then_some(result),
    };
    if ok && !names.is_empty() {
        tracing::info!(names = names.len(), "DNS name resources active");
    }
    set_state(s.clone());
    s
}

/// Windows and mobile: no names are routed; status says why when there
/// are some.
#[cfg(any(windows, target_os = "android", target_os = "ios"))]
fn unsupported(names: BTreeSet<String>) -> DnsState {
    name_map::with(|m| m.set_active(BTreeSet::new()));
    let s = DnsState {
        configured: BTreeSet::new(),
        status: (!names.is_empty())
            .then(|| {
                Err(format!(
                    "DNS name resources are not supported on {} yet",
                    if cfg!(windows) {
                        "Windows"
                    } else if cfg!(target_os = "ios") {
                        "iOS"
                    } else {
                        "Android"
                    }
                ))
            }),
    };
    set_state(s.clone());
    s
}

/// The tunnel went down: the link (and its resolved config) is gone.
pub(crate) fn clear() {
    name_map::with(|m| {
        m.set_active(BTreeSet::new());
        // The next bring-up records only the clashes its ACL still has.
        m.clear_conflicts();
    });
    set_state(DnsState::default());
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::AppliedEntry;
    use std::cell::RefCell;

    #[cfg_attr(any(windows, target_os = "android", target_os = "ios"), allow(dead_code))]
    struct Rec {
        calls: RefCell<Vec<String>>,
        fail_on: Option<&'static str>,
    }
    impl CmdRunner for Rec {
        fn run(&self, program: &str, args: &[&str], _stdin: Option<&str>) -> anyhow::Result<()> {
            let line = format!("{program} {}", args.join(" "));
            self.calls.borrow_mut().push(line.clone());
            if self.fail_on.is_some_and(|f| line.contains(f)) {
                anyhow::bail!("{line} failed with exit status: 1");
            }
            Ok(())
        }
        fn output(&self, _: &str, _: &[&str]) -> anyhow::Result<String> {
            Ok(String::new())
        }
    }

    fn cfg_of(entries: Vec<AppliedEntry>) -> AppliedConfig {
        AppliedConfig {
            spiffe_id: String::new(),
            certificate_pem: String::new(),
            private_key_pem: String::new(),
            tpm_key_material: None,
            ca_cert_pem: String::new(),
            entries,
        }
    }

    fn entry(address: &str, protocol: &str) -> AppliedEntry {
        AppliedEntry {
            address: address.into(),
            port: 443,
            protocol: protocol.into(),
            remote_network_id: "rn".into(),
            preferred_connector_id: String::new(),
            coords: vec![],
        }
    }

    #[test]
    fn names_are_the_tcp_name_entries() {
        let cfg = cfg_of(vec![
            entry("10.0.0.5", "tcp"),
            entry("db.acme.example", "tcp"),
            entry("web.acme.example", ""),
            entry("udp.acme.example", "udp"),
            entry("localhost", "tcp"),
        ]);
        let got: Vec<_> = names_in(&cfg).into_iter().collect();
        assert_eq!(got, vec!["db.acme.example", "web.acme.example"]);
    }

    #[test]
    fn commands_route_only_the_names_to_the_client() {
        let names: BTreeSet<String> = ["b.example".to_string(), "a.example".to_string()].into();
        let lines: Vec<String> = desired_commands(&names)
            .into_iter()
            .map(|(p, a)| format!("{p} {}", a.join(" ")))
            .collect();
        assert_eq!(
            lines,
            vec![
                "ip route replace 198.19.255.53/32 dev zecurity0",
                "resolvectl dns zecurity0 198.19.255.53",
                "resolvectl domain zecurity0 ~a.example ~b.example",
                "resolvectl default-route zecurity0 false",
            ]
        );
        assert_eq!(
            desired_commands(&BTreeSet::new())
                .into_iter()
                .map(|(p, a)| format!("{p} {}", a.join(" ")))
                .collect::<Vec<_>>(),
            vec!["resolvectl revert zecurity0"]
        );
    }

    // One test drives the process-wide state in sequence, so parallel tests
    // can't interleave on it.
    // The systemd-resolved path; Windows and mobile report names as unsupported.
    #[cfg(not(any(windows, target_os = "android", target_os = "ios")))]
    #[test]
    fn sync_tracks_names_and_reports_failures() {
        let ok = Rec {
            calls: RefCell::new(vec![]),
            fail_on: None,
        };
        let cfg = cfg_of(vec![entry("sync.acme.example", "tcp")]);
        let s = sync(&ok, &cfg, true);
        assert_eq!(s.status, Some(Ok(1)));
        assert_eq!(status_line().as_deref(), Some("active (1 name)"));
        let fake = name_map::resource_ipv4("sync.acme.example").unwrap();
        assert_eq!(
            name_map::with(|m| m.answer_a("sync.acme.example")),
            Some(fake)
        );

        // Unchanged names: nothing re-run.
        let before = ok.calls.borrow().len();
        sync(&ok, &cfg, false);
        assert_eq!(ok.calls.borrow().len(), before);

        // A resolved failure: names inactive, status says why, IP side untouched.
        let bad = Rec {
            calls: RefCell::new(vec![]),
            fail_on: Some("resolvectl dns"),
        };
        let s = sync(&bad, &cfg, true);
        assert!(
            matches!(s.status, Some(Err(ref r)) if r.contains("systemd-resolved configuration failed")),
            "{s:?}"
        );
        assert!(status_line().unwrap().starts_with("unavailable: "));
        assert_eq!(name_map::with(|m| m.answer_a("sync.acme.example")), None);

        // No names: revert, no status line.
        let s = sync(&ok, &cfg_of(vec![entry("10.0.0.5", "tcp")]), false);
        assert_eq!(s.status, None);
        assert_eq!(status_line(), None);
        assert!(ok
            .calls
            .borrow()
            .last()
            .unwrap()
            .starts_with("timeout 5 resolvectl revert"));

        clear();
        assert_eq!(status_line(), None);
    }
}
