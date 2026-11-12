// analytics.rs — real-time access log on stdout (PENDING-11 §9, W-2)
//
// Twingate-style: with ZECURITY_LOG_ANALYTICS=v1 the connector prints one
// line per access decision,
//
//   ANALYTICS {"schema":"zecurity.connector.analytics.v1", ...}
//
// to stdout, for the customer's log agent on the connector host (journald,
// Docker log) to collect. Off by default.
//
// - The line is written whole under the stdout lock, not through `tracing`,
//   so it carries no timestamp/level prefix and never interleaves with a
//   tracing line.
// - It is written whatever the state of the control stream, so the local copy
//   has no O-6 loss. It is not hash-chained: the chained copy is the
//   controller's `access` stream.
// - A failed write (closed stdout) is ignored; it must never stop a tunnel.
// - The write happens on its own thread behind a bounded queue: a log reader
//   that stops reading (a stalled journald or Docker log driver) fills the
//   queue and lines are dropped and counted, instead of a blocking stdout
//   write stalling the tunnel task that made the decision.

use std::io::Write;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::mpsc::{self, SyncSender, TrySendError};
use std::sync::OnceLock;

use serde::Serialize;
use time::format_description::well_known::Rfc3339;
use time::OffsetDateTime;

pub const SCHEMA: &str = "zecurity.connector.analytics.v1";
pub const LINE_PREFIX: &str = "ANALYTICS ";

/// Lines that may wait for the writer thread before new ones are dropped.
pub const QUEUE_CAPACITY: usize = 4096;

/// Set once at startup when analytics are on.
static ENABLED: OnceLock<Sink> = OnceLock::new();

/// Lines dropped because the queue was full, not yet reported.
static DROPPED: AtomicU64 = AtomicU64::new(0);

struct Sink {
    workspace_id: String,
    tx: SyncSender<String>,
}

/// Parses the ZECURITY_LOG_ANALYTICS setting. Unset or empty means off,
/// "v1" means on; anything else is an error (the caller logs it and stays off).
pub fn parse_setting(value: Option<&str>) -> Result<bool, String> {
    match value.map(str::trim) {
        None | Some("") => Ok(false),
        Some(v) if v.eq_ignore_ascii_case("v1") => Ok(true),
        Some(v) => Err(format!(
            "ZECURITY_LOG_ANALYTICS={v:?} is not supported (use \"v1\"); access analytics stay off"
        )),
    }
}

/// Turns the analytics lines on and starts the writer thread. Called once
/// from main after enrollment; later calls do nothing.
pub fn enable(workspace_id: &str) {
    if ENABLED.get().is_some() {
        return;
    }
    let (tx, rx) = mpsc::sync_channel::<String>(QUEUE_CAPACITY);
    let sink = Sink {
        workspace_id: workspace_id.to_owned(),
        tx,
    };
    if ENABLED.set(sink).is_err() {
        return;
    }
    let spawned = std::thread::Builder::new()
        .name("analytics-writer".into())
        .spawn(move || {
            let stdout = std::io::stdout();
            for line in rx {
                report_dropped();
                let mut out = stdout.lock();
                let _ = out.write_all(line.as_bytes());
                let _ = out.flush();
            }
        });
    if let Err(e) = spawned {
        // Lines then fill the queue and are dropped; tunnels are unaffected.
        tracing::warn!(error = %e, "access analytics writer thread did not start");
    }
}

/// Logs (from the writer thread, where blocking is harmless) how many lines
/// were dropped since the last report.
fn report_dropped() {
    let n = DROPPED.swap(0, Ordering::Relaxed);
    if n > 0 {
        tracing::warn!(
            dropped = n,
            "access analytics: stdout was not keeping up; lines were dropped"
        );
    }
}

/// Queues a full line (newline included) without blocking. Returns false
/// when it was dropped.
fn enqueue(tx: &SyncSender<String>, line: String) -> bool {
    match tx.try_send(line) {
        Ok(()) => true,
        Err(TrySendError::Full(_)) | Err(TrySendError::Disconnected(_)) => {
            DROPPED.fetch_add(1, Ordering::Relaxed);
            false
        }
    }
}

pub fn enabled() -> bool {
    ENABLED.get().is_some()
}

/// One access decision, as seen at the connector.
pub struct AccessDecision<'a> {
    pub connector_id: &'a str,
    pub client_spiffe_id: &'a str,
    pub resource_id: &'a str,
    pub route_type: &'a str,
    pub destination: &'a str,
    pub port: u16,
    pub protocol: &'a str,
    pub decision: &'a str, // "allow" | "deny" | "error"
    pub error: &'a str,
}

#[derive(Serialize)]
struct Line<'a> {
    schema: &'static str,
    event_type: &'static str,
    timestamp: String,
    decision: &'a str,
    #[serde(skip_serializing_if = "str::is_empty")]
    error: &'a str,
    workspace: Id<'a>,
    connector: Id<'a>,
    client: Client<'a>,
    resource: Resource<'a>,
    #[serde(skip_serializing_if = "str::is_empty")]
    route_type: &'a str,
}

#[derive(Serialize)]
struct Id<'a> {
    id: &'a str,
}

#[derive(Serialize)]
struct Client<'a> {
    spiffe_id: &'a str,
    /// "client" or "client_device", from spiffe://<trust-domain>/<kind>/<id>.
    #[serde(skip_serializing_if = "Option::is_none")]
    kind: Option<&'a str>,
    #[serde(skip_serializing_if = "Option::is_none")]
    id: Option<&'a str>,
}

#[derive(Serialize)]
struct Resource<'a> {
    #[serde(skip_serializing_if = "str::is_empty")]
    id: &'a str,
    destination: &'a str,
    port: u16,
    protocol: &'a str,
}

/// Splits spiffe://<trust-domain>/<kind>/<id> into (kind, id).
fn client_kind_id(spiffe_id: &str) -> Option<(&str, &str)> {
    let rest = spiffe_id.strip_prefix("spiffe://")?;
    let mut parts = rest.split('/');
    let (_td, kind, id) = (parts.next()?, parts.next()?, parts.next()?);
    if parts.next().is_some() || kind.is_empty() || id.is_empty() {
        return None;
    }
    Some((kind, id))
}

/// Builds the full line (prefix + JSON, no newline).
pub fn format_line(workspace_id: &str, d: &AccessDecision<'_>, now: OffsetDateTime) -> String {
    let kind_id = client_kind_id(d.client_spiffe_id);
    let line = Line {
        schema: SCHEMA,
        event_type: "access_decision",
        timestamp: now.format(&Rfc3339).unwrap_or_default(),
        decision: d.decision,
        error: d.error,
        workspace: Id { id: workspace_id },
        connector: Id { id: d.connector_id },
        client: Client {
            spiffe_id: d.client_spiffe_id,
            kind: kind_id.map(|(k, _)| k),
            id: kind_id.map(|(_, i)| i),
        },
        resource: Resource {
            id: d.resource_id,
            destination: d.destination,
            port: d.port,
            protocol: d.protocol,
        },
        route_type: d.route_type,
    };
    // Serializing these plain structs can't fail; fall back to nothing.
    match serde_json::to_string(&line) {
        Ok(json) => format!("{LINE_PREFIX}{json}"),
        Err(_) => String::new(),
    }
}

/// Queues the line for stdout when analytics are on. Never blocks.
pub fn emit(d: &AccessDecision<'_>) {
    let Some(sink) = ENABLED.get() else {
        return;
    };
    let mut line = format_line(&sink.workspace_id, d, OffsetDateTime::now_utc());
    if line.is_empty() {
        return;
    }
    line.push('\n');
    enqueue(&sink.tx, line);
}

#[cfg(test)]
mod tests {
    use super::*;

    const WS: &str = "616015cb-8c8f-49fc-a6ae-b50fbfdb1bf6";

    fn sample<'a>(decision: &'a str, error: &'a str, resource_id: &'a str) -> AccessDecision<'a> {
        AccessDecision {
            connector_id: "c0a80001-0000-4000-8000-000000000001",
            client_spiffe_id:
                "spiffe://ws-acme.zecurity.in/client_device/550e8400-e29b-41d4-a716-446655440000",
            resource_id,
            route_type: "direct",
            destination: "172.31.11.20",
            port: 9001,
            protocol: "tcp",
            decision,
            error,
        }
    }

    fn parse(line: &str) -> serde_json::Value {
        let json = line.strip_prefix(LINE_PREFIX).expect("ANALYTICS prefix");
        serde_json::from_str(json).expect("valid JSON")
    }

    #[test]
    fn setting_is_off_by_default_and_only_v1_turns_it_on() {
        assert_eq!(parse_setting(None), Ok(false));
        assert_eq!(parse_setting(Some("")), Ok(false));
        assert_eq!(parse_setting(Some("v1")), Ok(true));
        assert_eq!(parse_setting(Some(" V1 ")), Ok(true));
        assert!(parse_setting(Some("v3")).is_err());
        assert!(parse_setting(Some("true")).is_err());
    }

    #[test]
    fn allow_line_has_every_field() {
        let now = OffsetDateTime::from_unix_timestamp(1_793_000_000).unwrap();
        let line = format_line(WS, &sample("allow", "", "r-1"), now);
        assert!(!line.contains('\n'));
        let v = parse(&line);
        assert_eq!(v["schema"], SCHEMA);
        assert_eq!(v["event_type"], "access_decision");
        assert_eq!(v["timestamp"], "2026-10-26T07:33:20Z");
        assert_eq!(v["decision"], "allow");
        assert!(v.get("error").is_none());
        assert_eq!(v["workspace"]["id"], WS);
        assert_eq!(v["connector"]["id"], "c0a80001-0000-4000-8000-000000000001");
        assert_eq!(v["client"]["kind"], "client_device");
        assert_eq!(v["client"]["id"], "550e8400-e29b-41d4-a716-446655440000");
        assert_eq!(v["resource"]["id"], "r-1");
        assert_eq!(v["resource"]["destination"], "172.31.11.20");
        assert_eq!(v["resource"]["port"], 9001);
        assert_eq!(v["resource"]["protocol"], "tcp");
        assert_eq!(v["route_type"], "direct");
    }

    #[test]
    fn deny_without_resource_omits_the_empty_fields() {
        let now = OffsetDateTime::now_utc();
        let mut d = sample("deny", "no_acl_match", "");
        d.route_type = "";
        let v = parse(&format_line(WS, &d, now));
        assert_eq!(v["decision"], "deny");
        assert_eq!(v["error"], "no_acl_match");
        assert!(v["resource"].get("id").is_none());
        assert!(v.get("route_type").is_none());
    }

    #[test]
    fn hostile_values_stay_inside_the_json() {
        // A destination with quotes or a newline must not break the line.
        let now = OffsetDateTime::now_utc();
        let mut d = sample("error", "name_unresolved: \"x\"\nANALYTICS {}", "r-1");
        d.destination = "evil\"\n.example";
        let line = format_line(WS, &d, now);
        assert_eq!(line.lines().count(), 1);
        let v = parse(&line);
        assert_eq!(v["resource"]["destination"], "evil\"\n.example");
    }

    #[test]
    fn full_queue_drops_and_counts_instead_of_blocking() {
        // No reader: the queue fills and further lines are dropped at once.
        let (tx, _rx) = mpsc::sync_channel::<String>(2);
        let before = DROPPED.load(Ordering::Relaxed);
        assert!(enqueue(&tx, "a\n".into()));
        assert!(enqueue(&tx, "b\n".into()));
        assert!(!enqueue(&tx, "c\n".into()));
        assert!(!enqueue(&tx, "d\n".into()));
        assert!(DROPPED.load(Ordering::Relaxed) >= before + 2);
    }

    #[test]
    fn odd_spiffe_ids_keep_the_raw_id_only() {
        let now = OffsetDateTime::now_utc();
        for id in [
            "",
            "spiffe://td",
            "spiffe://td/client",
            "spiffe://td/a/b/c",
            "https://x/a/b",
        ] {
            let mut d = sample("deny", "x", "");
            d.client_spiffe_id = id;
            let v = parse(&format_line(WS, &d, now));
            assert_eq!(v["client"]["spiffe_id"], id);
            assert!(v["client"].get("kind").is_none(), "{id}");
        }
    }
}
