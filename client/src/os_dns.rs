// os_dns.rs — the daemon's client for the privileged DNS helper (ADR-023 option C).
//
// The daemon is unprivileged and cannot configure per-link DNS itself: those
// systemd-resolved actions are polkit-gated behind `auth_admin`, and capabilities do
// not help because polkit authorizes on uid. So it asks `zecurity-dns-helper`, which
// runs as root, validates everything, and refuses anything outside its whitelist.
//
// FAIL SOFT, ALWAYS. Every call here is best-effort. If the helper is not installed,
// not running, or refuses, the tunnel must keep working — resources stay reachable by
// synthetic IP or a `hosts` entry (the Phase 9.5 path). Making DNS integration a hard
// dependency of connectivity would trade a working tunnel for a convenience.
//
// This module holds NO state and makes no policy: the domain list is derived from
// `RuntimeState.synthetic_bindings` on each call, so it cannot drift from the registry.

use std::collections::HashMap;
use std::net::Ipv4Addr;
use std::path::Path;
use std::time::Duration;

use serde::{Deserialize, Serialize};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::UnixStream;
use tracing::{debug, info, warn};

/// Matches `ListenStream=` in `dns-helper/systemd/zecurity-dns-helper.socket`.
pub const HELPER_SOCKET: &str = "/run/zecurity-dns-helper.sock";

/// The interface we configure — and the only one the helper will accept.
pub const IFACE: &str = "zecurity0";

/// A helper call is a local socket round-trip; if it has not answered in this long
/// something is wrong and we would rather carry on without DNS than block a tunnel
/// bring-up.
const TIMEOUT: Duration = Duration::from_secs(3);

#[derive(Debug, Serialize)]
#[serde(tag = "verb", rename_all = "snake_case")]
enum Request<'a> {
    Apply {
        iface: &'a str,
        server: &'a str,
        domains: Vec<String>,
    },
    Revert {
        iface: &'a str,
    },
}

#[derive(Debug, Deserialize)]
struct Response {
    ok: bool,
    #[serde(default)]
    error: Option<String>,
}

/// Turn the live registry into the helper's `domains` argument.
///
/// One **routing-only** entry per managed FQDN — never a shared parent. ADR-023's
/// Task 1 showed `~domain` captures the whole subtree, so routing a suffix would hand
/// our responder every sibling name; it exact-matches and returns REFUSED, and because
/// the route is link-scoped resolved has nowhere else to try. That would break names we
/// do not manage. The helper enforces this too (>= 2 labels), but building it correctly
/// here means we never send something it must reject.
///
/// Sorted so repeated calls with an unchanged registry produce an identical request —
/// `HashMap` iteration order would otherwise make every call look like a change.
pub fn routing_domains(bindings: &HashMap<String, Ipv4Addr>) -> Vec<String> {
    let mut v: Vec<String> = bindings
        .keys()
        .map(|h| h.trim())
        .filter(|h| !h.is_empty())
        .map(|h| format!("~{h}"))
        .collect();
    v.sort();
    v.dedup();
    v
}

/// Point the TUN's resolver at our responder and route exactly these names to it.
///
/// An empty `bindings` is meaningful: it tells the helper to route nothing, which it
/// implements as a revert. That is how "the last name-addressed resource went away" is
/// expressed without a separate call.
pub async fn apply(bindings: &HashMap<String, Ipv4Addr>, server_ip: &str) -> bool {
    let domains = routing_domains(bindings);
    let n = domains.len();
    match call(
        Path::new(HELPER_SOCKET),
        &Request::Apply {
            iface: IFACE,
            server: server_ip,
            domains,
        },
    )
    .await
    {
        Ok(()) => {
            info!(
                iface = IFACE,
                server = server_ip,
                domains = n,
                "OS DNS routing applied"
            );
            true
        }
        Err(e) => {
            soft_fail("apply", &e);
            false
        }
    }
}

/// Drop our per-link DNS configuration.
///
/// Nearly always redundant: the configuration lives on `zecurity0`, and a non-persistent
/// TUN disappears with the process, taking resolved's per-link state with it. Called
/// anyway on `down` and at startup so the one case that *can* strand it — a TUN that
/// outlived its creator — is still cleaned. Idempotent by contract.
pub async fn revert() -> bool {
    match call(Path::new(HELPER_SOCKET), &Request::Revert { iface: IFACE }).await {
        Ok(()) => {
            info!(iface = IFACE, "OS DNS routing reverted");
            true
        }
        Err(e) => {
            soft_fail("revert", &e);
            false
        }
    }
}

/// One log line, one shape, so an absent helper is never mistaken for a bug in the
/// tunnel — and so it is loud enough to notice, since a silently missing helper simply
/// looks like "DNS does not work".
fn soft_fail(verb: &str, e: &anyhow::Error) {
    if let Some(io) = e.downcast_ref::<std::io::Error>() {
        if io.kind() == std::io::ErrorKind::NotFound
            || io.kind() == std::io::ErrorKind::ConnectionRefused
        {
            warn!(
                socket = HELPER_SOCKET,
                "DNS helper unavailable — managed names will NOT resolve automatically. \
                 The tunnel is unaffected: reach resources by synthetic IP or add a hosts \
                 entry (`zecurity-client resources` prints both). Install/enable \
                 zecurity-dns-helper.socket to fix."
            );
            return;
        }
    }
    warn!(verb, error = %e, "DNS helper call failed — continuing without OS DNS integration");
}

async fn call(socket: &Path, req: &Request<'_>) -> anyhow::Result<()> {
    let body = serde_json::to_string(req)?;
    let fut = async {
        let stream = UnixStream::connect(socket).await?;
        let (r, mut w) = stream.into_split();
        w.write_all(format!("{body}\n").as_bytes()).await?;
        w.flush().await?;
        let mut line = String::new();
        BufReader::new(r).read_line(&mut line).await?;
        Ok::<String, anyhow::Error>(line)
    };
    let line = tokio::time::timeout(TIMEOUT, fut)
        .await
        .map_err(|_| anyhow::anyhow!("helper did not respond within {TIMEOUT:?}"))??;

    let resp: Response = serde_json::from_str(line.trim())
        .map_err(|e| anyhow::anyhow!("unparseable helper response {:?}: {e}", line.trim()))?;
    if resp.ok {
        debug!("helper accepted the request");
        Ok(())
    } else {
        anyhow::bail!(
            "helper refused: {}",
            resp.error.unwrap_or_else(|| "no reason given".into())
        )
    }
}

/// Only used to make the socket path overridable in tests.
#[cfg(test)]
async fn call_at(socket: std::path::PathBuf, req: &Request<'_>) -> anyhow::Result<()> {
    call(&socket, req).await
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;
    use tokio::net::UnixListener;

    fn binds(pairs: &[(&str, &str)]) -> HashMap<String, Ipv4Addr> {
        pairs
            .iter()
            .map(|(h, ip)| (h.to_string(), ip.parse().unwrap()))
            .collect()
    }

    #[test]
    fn each_managed_fqdn_becomes_its_own_routing_only_domain() {
        let d = routing_domains(&binds(&[
            ("app.internal", "100.64.0.2"),
            ("db.internal", "100.64.0.3"),
        ]));
        assert_eq!(d, vec!["~app.internal", "~db.internal"]);
    }

    /// Never a shared parent: `~internal` would capture sibling names we do not manage.
    #[test]
    fn no_parent_domain_is_ever_produced() {
        let d = routing_domains(&binds(&[("app.internal", "100.64.0.2")]));
        assert!(!d.contains(&"~internal".to_string()));
        for x in &d {
            let labels = x.trim_start_matches('~').split('.').count();
            assert!(labels >= 2, "{x} must be a full FQDN, not a parent");
        }
    }

    /// A `HashMap` iterates in arbitrary order; an unsorted list would make every call
    /// look like a change to anything comparing requests.
    #[test]
    fn the_order_is_deterministic() {
        let b = binds(&[
            ("z.internal", "100.64.0.9"),
            ("a.internal", "100.64.0.2"),
            ("m.internal", "100.64.0.5"),
        ]);
        let first = routing_domains(&b);
        for _ in 0..20 {
            assert_eq!(routing_domains(&b), first);
        }
        assert_eq!(first, vec!["~a.internal", "~m.internal", "~z.internal"]);
    }

    #[test]
    fn an_empty_registry_yields_an_empty_list_meaning_route_nothing() {
        assert!(routing_domains(&HashMap::new()).is_empty());
    }

    #[test]
    fn blank_hostnames_are_skipped_rather_than_sent_as_a_bare_tilde() {
        let mut b = binds(&[("app.internal", "100.64.0.2")]);
        b.insert("   ".into(), "100.64.0.3".parse().unwrap());
        b.insert("".into(), "100.64.0.4".parse().unwrap());
        let d = routing_domains(&b);
        assert_eq!(d, vec!["~app.internal"]);
        assert!(!d.iter().any(|x| x == "~"), "'~' alone would route ALL DNS");
    }

    // ── transport, against a stub helper on a real socket ────────────────────

    async fn stub(dir: &Path, reply: &'static str) -> (PathBuf, tokio::task::JoinHandle<String>) {
        let path = dir.join("helper.sock");
        let l = UnixListener::bind(&path).unwrap();
        let h = tokio::spawn(async move {
            let (s, _) = l.accept().await.unwrap();
            let (r, mut w) = s.into_split();
            let mut line = String::new();
            BufReader::new(r).read_line(&mut line).await.unwrap();
            w.write_all(format!("{reply}\n").as_bytes()).await.unwrap();
            line
        });
        (path, h)
    }

    fn tmp(tag: &str) -> PathBuf {
        let d = std::env::temp_dir().join(format!("zc-osdns-{tag}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&d);
        std::fs::create_dir_all(&d).unwrap();
        d
    }

    #[tokio::test]
    async fn a_successful_apply_sends_the_expected_request() {
        let d = tmp("ok");
        let (path, h) = stub(&d, r#"{"ok":true}"#).await;
        let req = Request::Apply {
            iface: IFACE,
            server: "127.0.0.1",
            domains: routing_domains(&binds(&[("app.internal", "100.64.0.2")])),
        };
        call_at(path, &req).await.expect("should succeed");
        let sent = h.await.unwrap();
        assert!(sent.contains(r#""verb":"apply""#), "got {sent}");
        assert!(sent.contains(r#""iface":"zecurity0""#), "got {sent}");
        assert!(sent.contains(r#""~app.internal""#), "got {sent}");
        let _ = std::fs::remove_dir_all(&d);
    }

    #[tokio::test]
    async fn a_refusal_is_surfaced_with_its_reason() {
        let d = tmp("refuse");
        let (path, _h) = stub(&d, r#"{"ok":false,"error":"refusing to configure eth0"}"#).await;
        let err = call_at(path, &Request::Revert { iface: IFACE })
            .await
            .expect_err("should fail");
        assert!(err.to_string().contains("eth0"), "got {err}");
        let _ = std::fs::remove_dir_all(&d);
    }

    /// The case that must never take the tunnel down: no helper installed at all.
    #[tokio::test]
    async fn an_absent_helper_is_an_error_not_a_panic() {
        let err = call_at(
            PathBuf::from("/run/definitely-not-here.sock"),
            &Request::Revert { iface: IFACE },
        )
        .await
        .expect_err("should fail");
        let io = err.downcast_ref::<std::io::Error>().expect("io error");
        assert_eq!(io.kind(), std::io::ErrorKind::NotFound);
    }

    #[tokio::test]
    async fn garbage_from_the_helper_is_rejected_not_trusted() {
        let d = tmp("garbage");
        let (path, _h) = stub(&d, "this is not json").await;
        let err = call_at(path, &Request::Revert { iface: IFACE })
            .await
            .expect_err("should fail");
        assert!(err.to_string().contains("unparseable"), "got {err}");
        let _ = std::fs::remove_dir_all(&d);
    }
}
