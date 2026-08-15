use std::borrow::Cow;
use std::collections::hash_map::DefaultHasher;
use std::hash::{Hash, Hasher};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use nftables::{
    batch::Batch,
    expr::{Expression, Meta, MetaKey, NamedExpression, Payload, PayloadField, Prefix, Range},
    helper,
    schema::{Chain, FlushObject, NfCmd, NfListObject, Rule, Table},
    stmt::{Accept, Drop, Match, Operator, Statement},
    types::{NfChainPolicy, NfChainType, NfFamily, NfHook},
};
use tracing::{error, info, warn};

use crate::proto::ResourceAck;
use crate::util;

const TABLE: &str = "zecurity";
const PROTECT_CHAIN: &str = "resource_protect";

#[derive(Clone)]
pub struct ActiveResource {
    pub resource_id: String,
    pub host: String,
    /// The validated local address this shield dials and health-probes for this
    /// resource: the instruction's `local_target` when the controller set one,
    /// else `host`. Resolved and checked ONCE at apply time (see `dial_target`)
    /// so the tunnel and health paths cannot re-derive it differently.
    pub dial_target: String,
    pub protocol: String,
    pub port_from: u16,
    pub port_to: u16,
}

pub struct SharedResourceState {
    pub active: Mutex<Vec<ActiveResource>>,
    pub acks: Mutex<Vec<ResourceAck>>,
    /// Generation of the last applied ResourceSnapshot (ADR-004 Phase 2).
    /// Stale snapshots (e.g. a cached replay racing a newer live push) are
    /// dropped so an older truth can never overwrite a newer one.
    pub last_snapshot_generation: Mutex<u64>,
    /// Session-local sequence, bumped on every active-set mutation (ADR-004
    /// Phase 3). Lets the controller order state reports within a session.
    pub state_seq: Mutex<u64>,
}

impl SharedResourceState {
    pub fn new() -> Self {
        Self {
            active: Mutex::new(Vec::new()),
            acks: Mutex::new(Vec::new()),
            last_snapshot_generation: Mutex::new(0),
            state_seq: Mutex::new(0),
        }
    }

    pub fn store_ack(&self, ack: ResourceAck) {
        let mut acks = self.acks.lock().unwrap();
        acks.retain(|a| a.resource_id != ack.resource_id);
        acks.push(ack);
    }

    pub fn drain_acks(&self) -> Vec<ResourceAck> {
        let mut acks = self.acks.lock().unwrap();
        std::mem::take(&mut *acks)
    }

    pub fn bump_state_seq(&self) {
        *self.state_seq.lock().unwrap() += 1;
    }

    /// The address this shield dials for `resource_id`, taken from its OWN applied
    /// state (Sprint 16 Phase 8.2).
    ///
    /// This exists so the tunnel path never takes a dial target out of the
    /// per-connection `TunnelOpen` message. The connector asserts an identity; the
    /// shield decides the address — the same split Stage 1 established one layer up,
    /// where the client asserts a `resource_id` and the connector dials its own ACL's
    /// address. Reading an address from the message instead would hand the connector
    /// free-form dialing inside the shield.
    ///
    /// `None` means this shield has not applied that resource, so it has no opinion
    /// about where to dial — the caller decides what to do with that.
    pub fn dial_target_for(&self, resource_id: &str) -> Option<String> {
        self.active
            .lock()
            .unwrap()
            .iter()
            .find(|r| r.resource_id == resource_id)
            .map(|r| r.dial_target.clone())
    }

    /// Build the actual-state report from the in-memory active set (ADR-004
    /// Phase 3). Reflects the shield's applied intent — not raw kernel state.
    pub fn build_state_report(&self, shield_id: &str) -> crate::proto::ResourceStateReport {
        let mut ids: Vec<String> = self
            .active
            .lock()
            .unwrap()
            .iter()
            .map(|r| r.resource_id.clone())
            .collect();
        ids.sort();
        let mut hasher = DefaultHasher::new();
        for id in &ids {
            id.hash(&mut hasher);
        }
        crate::proto::ResourceStateReport {
            shield_id: shield_id.to_string(),
            generation: *self.state_seq.lock().unwrap(),
            active_resource_ids: ids,
            fingerprint: hasher.finish(),
        }
    }
}

pub fn validate_host(resource_host: &str) -> bool {
    if resource_host == "127.0.0.1" {
        return true;
    }
    match util::detect_lan_ip() {
        Some(my_ip) => my_ip == resource_host,
        None => false,
    }
}

/// Why a resource was refused, naming the field the bad value came from.
///
/// Without this, a `local_target` typo surfaced as "resource host does not match
/// this shield's LAN IP" — pointing the operator at the wrong column.
/// `Debug` so the tests can `unwrap_err()`, and so a rejection is printable if it
/// ever reaches an error chain rather than only the structured log.
#[derive(Debug)]
pub struct RejectedTarget {
    /// "host" or "local_target".
    pub field: &'static str,
    pub value: String,
}

/// Which local address this shield will dial for a resource, or why it won't.
///
/// `local_target` SELECTS FROM the allowed set — it never extends it. Both values
/// go through `validate_host`, so the allowed set is exactly what it was before
/// Phase 8: loopback, or this shield's own LAN IP. Never a hostname: the shield is
/// not a resolver, and accepting names here would put one inside it.
///
/// `host` is validated even when `local_target` overrides the dial address,
/// because `host` is what binds this resource to THIS shield. Skipping it would
/// let an instruction naming another shield's LAN IP be applied here as long as it
/// carried `local_target: "127.0.0.1"`.
///
/// Empty `local_target` → dial `host`, i.e. the pre-Phase-8 behaviour, so an
/// un-upgraded controller and a resource with no local_target are identical.
pub fn dial_target<'a>(host: &'a str, local_target: &'a str) -> Result<&'a str, RejectedTarget> {
    if !validate_host(host) {
        return Err(RejectedTarget {
            field: "host",
            value: host.to_string(),
        });
    }
    let candidate = local_target.trim();
    if candidate.is_empty() {
        return Ok(host);
    }
    if !validate_host(candidate) {
        return Err(RejectedTarget {
            field: "local_target",
            value: candidate.to_string(),
        });
    }
    Ok(candidate)
}

pub fn check_port(host: &str, port: u16) -> bool {
    // Hosts reaching here are validated IPs (127.0.0.1 or detect_lan_ip()), so this
    // parses in practice — but never panic on a malformed address: an unparseable
    // host is simply treated as not-listening (fail to `failed`, not a shield crash).
    let addr = match format!("{}:{}", host, port).parse::<std::net::SocketAddr>() {
        Ok(addr) => addr,
        Err(_) => {
            warn!(
                host = host,
                port = port,
                "check_port: unparseable address, treating as unreachable"
            );
            return false;
        }
    };
    std::net::TcpStream::connect_timeout(&addr, Duration::from_secs(2)).is_ok()
}

/// Build the single atomic nftables transaction that (re)builds `resource_protect`.
/// Split out from application so the command ordering — critically, `flush` BEFORE the
/// rule adds — is unit-testable without a kernel.
///
/// The whole thing is ONE transaction, so it commits as a swap: the old drop rules stay
/// in force until the kernel atomically replaces them with the new ruleset. This is the
/// security-critical property (F21) — PREPARE the next state, then commit; never
/// "destroy old protection, then build new". The previous version deleted the chain in
/// one transaction and re-added it in a second, which (a) left a fail-open window on
/// every rebuild while the chain was absent, and (b) — far worse — left every resource
/// unprotected if the re-add failed after the delete committed, while in-memory state
/// still reported them protected (control/data-plane divergence).
///
/// `add table`/`add chain` are idempotent (no-op if present; also recover if
/// network::setup() failed); `flush chain` clears the old rules in the same commit while
/// the chain object — and thus its Input hook and Accept policy — stays installed
/// throughout, so enforcement never lapses.
fn build_protect_ruleset(resources: &[ActiveResource]) -> nftables::schema::Nftables<'static> {
    let mut batch = Batch::new();
    batch.add(NfListObject::Table(Table {
        family: NfFamily::INet,
        name: TABLE.into(),
        ..Table::default()
    }));
    batch.add(NfListObject::Chain(Chain {
        family: NfFamily::INet,
        table: TABLE.into(),
        name: PROTECT_CHAIN.into(),
        _type: Some(NfChainType::Filter),
        hook: Some(NfHook::Input),
        prio: Some(10),
        policy: Some(NfChainPolicy::Accept),
        ..Chain::default()
    }));
    // Clear existing rules in the SAME transaction. `add chain; flush chain` is the
    // standard atomic-reload idiom. ORDERING IS LOAD-BEARING: this flush MUST precede
    // the rule adds below — flushing after would wipe the new rules and leave the chain
    // empty (fail-open). Guarded by `flush_precedes_rule_adds`.
    batch.add_cmd(NfCmd::Flush(FlushObject::Chain(Chain {
        family: NfFamily::INet,
        table: TABLE.into(),
        name: PROTECT_CHAIN.into(),
        ..Chain::default()
    })));

    for res in resources {
        let protos: &[&str] = match res.protocol.as_str() {
            "tcp" => &["tcp"],
            "udp" => &["udp"],
            _ => &["tcp", "udp"],
        };
        for proto in protos {
            let port_expr = port_expression(res.port_from, res.port_to);
            batch.add(NfListObject::Rule(iif_accept_rule(
                proto,
                port_expr.clone(),
                "lo",
            )));
            batch.add(NfListObject::Rule(source_accept_rule(
                proto,
                port_expr.clone(),
                "127.0.0.0/8",
            )));
            batch.add(NfListObject::Rule(port_drop_rule(proto, port_expr)));
            info!(
                resource_id = %res.resource_id,
                proto = proto,
                port = res.port_from,
                rules = "lo,localhost-source,drop",
                "firewall rules applied",
            );
        }
    }

    batch.to_nftables()
}

/// Atomically (re)build `chain resource_protect` and commit it to the kernel in a
/// single nftables transaction. See build_protect_ruleset for the atomicity rationale.
pub async fn apply_nftables(resources: &[ActiveResource]) -> Result<()> {
    helper::apply_ruleset_async(&build_protect_ruleset(resources))
        .await
        .context("failed to apply resource_protect chain")?;

    info!(
        resource_count = resources.len(),
        "rebuilt nftables resource_protect chain (atomic flush+rebuild)"
    );
    Ok(())
}

pub async fn run_health_check_loop(interval_secs: u64, state: Arc<SharedResourceState>) {
    let mut ticker = tokio::time::interval(Duration::from_secs(interval_secs));
    loop {
        ticker.tick().await;

        let resources: Vec<ActiveResource> = state.active.lock().unwrap().clone();
        if resources.is_empty() {
            continue;
        }

        let now = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_secs() as i64;

        let mut fresh_acks: Vec<ResourceAck> = resources
            .iter()
            .map(|res| {
                let reachable = check_port(&res.dial_target, res.port_from);
                ResourceAck {
                    resource_id: res.resource_id.clone(),
                    status: if reachable {
                        "protected".to_string()
                    } else {
                        "failed".to_string()
                    },
                    error: if reachable {
                        String::new()
                    } else {
                        "port not listening".to_string()
                    },
                    verified_at: now,
                    port_reachable: reachable,
                }
            })
            .collect();

        let mut acks = state.acks.lock().unwrap();
        for ack in fresh_acks.drain(..) {
            acks.retain(|a| a.resource_id != ack.resource_id);
            acks.push(ack);
        }
    }
}

pub async fn handle_instruction(
    instruction: &crate::proto::ResourceInstruction,
    state: &Arc<SharedResourceState>,
) -> Option<ResourceAck> {
    match instruction.action.as_str() {
        "apply" => Some(handle_apply(instruction, state).await),
        "remove" => Some(handle_remove(instruction, state).await),
        other => {
            warn!(
                action = other,
                resource_id = %instruction.resource_id,
                "unknown resource action"
            );
            None
        }
    }
}

pub async fn handle_apply(
    instruction: &crate::proto::ResourceInstruction,
    state: &Arc<SharedResourceState>,
) -> ResourceAck {
    let now = now_unix();

    let target = match dial_target(&instruction.host, &instruction.local_target) {
        Ok(t) => t.to_string(),
        Err(rej) => {
            warn!(
                resource_id = %instruction.resource_id,
                field = rej.field,
                value = %rej.value,
                "rejecting resource — not an address this shield may dial \
                 (allowed: 127.0.0.1 or this shield's LAN IP)"
            );
            return ResourceAck {
                resource_id: instruction.resource_id.clone(),
                status: "failed".to_string(),
                error: format!(
                    "{} {:?} is not an address this shield may dial",
                    rej.field, rej.value
                ),
                verified_at: now,
                port_reachable: false,
            };
        }
    };

    {
        let mut active = state.active.lock().unwrap();
        if let Some(existing) = active
            .iter_mut()
            .find(|r| r.resource_id == instruction.resource_id)
        {
            existing.host = instruction.host.clone();
            // Must be assigned here too. handle_snapshot rebuilds its Vec from
            // scratch and would pick a new value up regardless; this branch mutates
            // in place, so omitting it means a local_target change delivered as an
            // incremental push keeps the stale target while a full resync fixes it
            // — "works after a resync but not after a push".
            existing.dial_target = target.clone();
            existing.protocol = instruction.protocol.clone();
            existing.port_from = instruction.port_from as u16;
            existing.port_to = instruction.port_to as u16;
        } else {
            active.push(ActiveResource {
                resource_id: instruction.resource_id.clone(),
                host: instruction.host.clone(),
                dial_target: target.clone(),
                protocol: instruction.protocol.clone(),
                port_from: instruction.port_from as u16,
                port_to: instruction.port_to as u16,
            });
        }
    }
    state.bump_state_seq();

    let snapshot = state.active.lock().unwrap().clone();
    match apply_nftables(&snapshot).await {
        Ok(()) => {
            let reachable = check_port(&target, instruction.port_from as u16);
            info!(
                resource_id = %instruction.resource_id,
                dial_target = %target,
                port = instruction.port_from,
                port_reachable = reachable,
                "resource applied — nftables chain rebuilt"
            );
            ResourceAck {
                resource_id: instruction.resource_id.clone(),
                status: if reachable {
                    "protected".to_string()
                } else {
                    "failed".to_string()
                },
                error: if reachable {
                    String::new()
                } else {
                    "port not listening".to_string()
                },
                verified_at: now,
                port_reachable: reachable,
            }
        }
        Err(e) => {
            state
                .active
                .lock()
                .unwrap()
                .retain(|r| r.resource_id != instruction.resource_id);
            state.bump_state_seq();
            error!(
                resource_id = %instruction.resource_id,
                error = %e,
                "nftables apply failed"
            );
            ResourceAck {
                resource_id: instruction.resource_id.clone(),
                status: "failed".to_string(),
                error: e.to_string(),
                verified_at: now,
                port_reachable: false,
            }
        }
    }
}

pub async fn handle_remove(
    instruction: &crate::proto::ResourceInstruction,
    state: &Arc<SharedResourceState>,
) -> ResourceAck {
    state
        .active
        .lock()
        .unwrap()
        .retain(|r| r.resource_id != instruction.resource_id);
    state.bump_state_seq();

    let snapshot = state.active.lock().unwrap().clone();
    if let Err(e) = apply_nftables(&snapshot).await {
        error!(
            resource_id = %instruction.resource_id,
            error = %e,
            "nftables rebuild after remove failed"
        );
    }

    info!(
        resource_id = %instruction.resource_id,
        "resource removed from nftables"
    );
    ResourceAck {
        resource_id: instruction.resource_id.clone(),
        status: "unprotected".to_string(),
        error: String::new(),
        verified_at: now_unix(),
        port_reachable: false,
    }
}

/// Apply an authoritative desired-state snapshot (ADR-004 Phase 2):
/// replace the active set with exactly the snapshot contents and rebuild the
/// chain — anything absent is dropped, anything missing is added. Acks every
/// resource so the controller's protecting→protected transitions still happen
/// when an apply was lost and the snapshot re-asserted it. Resources dropped
/// by omission get no ack (explicit removes still arrive as instructions).
pub async fn handle_snapshot(
    snapshot: &crate::proto::ResourceSnapshot,
    state: &Arc<SharedResourceState>,
) -> Vec<ResourceAck> {
    // Monotonic-apply guard: never let an older truth overwrite a newer one.
    {
        let mut last = state.last_snapshot_generation.lock().unwrap();
        if snapshot.generation <= *last {
            warn!(
                generation = snapshot.generation,
                last_applied = *last,
                "ignoring stale resource snapshot"
            );
            return Vec::new();
        }
        *last = snapshot.generation;
    }

    let now = now_unix();
    let mut acks = Vec::new();
    let mut new_active = Vec::new();
    for res in &snapshot.resources {
        let target = match dial_target(&res.host, &res.local_target) {
            Ok(t) => t.to_string(),
            Err(rej) => {
                warn!(
                    resource_id = %res.resource_id,
                    field = rej.field,
                    value = %rej.value,
                    "snapshot resource skipped — not an address this shield may dial"
                );
                acks.push(ResourceAck {
                    resource_id: res.resource_id.clone(),
                    status: "failed".to_string(),
                    error: format!(
                        "{} {:?} is not an address this shield may dial",
                        rej.field, rej.value
                    ),
                    verified_at: now,
                    port_reachable: false,
                });
                continue;
            }
        };
        new_active.push(ActiveResource {
            resource_id: res.resource_id.clone(),
            host: res.host.clone(),
            dial_target: target,
            protocol: res.protocol.clone(),
            port_from: res.port_from as u16,
            port_to: res.port_to as u16,
        });
    }

    // The replace: active becomes exactly the snapshot's (validated) contents.
    *state.active.lock().unwrap() = new_active;
    state.bump_state_seq();
    let applied = state.active.lock().unwrap().clone();
    match apply_nftables(&applied).await {
        Ok(()) => {
            info!(
                resource_count = applied.len(),
                generation = snapshot.generation,
                "resource snapshot applied — chain rebuilt"
            );
            for r in &applied {
                let reachable = check_port(&r.dial_target, r.port_from);
                acks.push(ResourceAck {
                    resource_id: r.resource_id.clone(),
                    status: if reachable { "protected" } else { "failed" }.to_string(),
                    error: if reachable {
                        String::new()
                    } else {
                        "port not listening".to_string()
                    },
                    verified_at: now,
                    port_reachable: reachable,
                });
            }
        }
        Err(e) => {
            error!(error = %e, "snapshot nftables apply failed");
            for r in &applied {
                acks.push(ResourceAck {
                    resource_id: r.resource_id.clone(),
                    status: "failed".to_string(),
                    error: e.to_string(),
                    verified_at: now,
                    port_reachable: false,
                });
            }
        }
    }
    acks
}

fn now_unix() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs() as i64
}

fn port_expression(port_from: u16, port_to: u16) -> Expression<'static> {
    if port_from == port_to {
        Expression::Number(port_from as u32)
    } else {
        Expression::Range(Box::new(Range {
            range: [
                Expression::Number(port_from as u32),
                Expression::Number(port_to as u32),
            ],
        }))
    }
}

fn iif_accept_rule(protocol: &str, port_expr: Expression<'static>, iif: &str) -> Rule<'static> {
    Rule {
        family: NfFamily::INet,
        table: TABLE.into(),
        chain: PROTECT_CHAIN.into(),
        expr: Cow::Owned(vec![
            Statement::Match(Match {
                left: Expression::Named(NamedExpression::Meta(Meta {
                    key: MetaKey::Iifname,
                })),
                right: Expression::String(Cow::Owned(iif.to_string())),
                op: Operator::EQ,
            }),
            Statement::Match(Match {
                left: Expression::Named(NamedExpression::Payload(Payload::PayloadField(
                    PayloadField {
                        protocol: Cow::Owned(protocol.to_string()),
                        field: "dport".into(),
                    },
                ))),
                right: port_expr,
                op: Operator::EQ,
            }),
            Statement::Accept(Some(Accept {})),
        ]),
        ..Rule::default()
    }
}

fn source_accept_rule(
    protocol: &str,
    port_expr: Expression<'static>,
    source: &str,
) -> Rule<'static> {
    // Parse "addr/len" into a Prefix expression. Fall back to plain string for
    // single-host addresses (no slash), which nftables resolves correctly. A
    // malformed prefix length also falls back to the plain-string form rather than
    // panicking — today `source` is always the hardcoded "127.0.0.0/8", but never let
    // a bad rule string crash the shield mid-apply.
    let source_expr: Expression<'static> = match source.split_once('/') {
        Some((addr, len)) => match len.parse::<u32>() {
            Ok(len) => Expression::Named(NamedExpression::Prefix(Prefix {
                addr: Box::new(Expression::String(Cow::Owned(addr.to_string()))),
                len,
            })),
            Err(_) => {
                warn!(
                    source = source,
                    "invalid prefix length in source rule, using literal"
                );
                Expression::String(Cow::Owned(source.to_string()))
            }
        },
        None => Expression::String(Cow::Owned(source.to_string())),
    };

    Rule {
        family: NfFamily::INet,
        table: TABLE.into(),
        chain: PROTECT_CHAIN.into(),
        expr: Cow::Owned(vec![
            Statement::Match(Match {
                left: Expression::Named(NamedExpression::Payload(Payload::PayloadField(
                    PayloadField {
                        protocol: "ip".into(),
                        field: "saddr".into(),
                    },
                ))),
                right: source_expr,
                op: Operator::EQ,
            }),
            Statement::Match(Match {
                left: Expression::Named(NamedExpression::Payload(Payload::PayloadField(
                    PayloadField {
                        protocol: Cow::Owned(protocol.to_string()),
                        field: "dport".into(),
                    },
                ))),
                right: port_expr,
                op: Operator::EQ,
            }),
            Statement::Accept(Some(Accept {})),
        ]),
        ..Rule::default()
    }
}

fn port_drop_rule(protocol: &str, port_expr: Expression<'static>) -> Rule<'static> {
    Rule {
        family: NfFamily::INet,
        table: TABLE.into(),
        chain: PROTECT_CHAIN.into(),
        expr: Cow::Owned(vec![
            Statement::Match(Match {
                left: Expression::Named(NamedExpression::Payload(Payload::PayloadField(
                    PayloadField {
                        protocol: Cow::Owned(protocol.to_string()),
                        field: "dport".into(),
                    },
                ))),
                right: port_expr,
                op: Operator::EQ,
            }),
            Statement::Drop(Some(Drop {})),
        ]),
        ..Rule::default()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use nftables::schema::NfObject;

    fn res(id: &str, port: u16) -> ActiveResource {
        ActiveResource {
            resource_id: id.to_string(),
            host: "10.0.0.1".to_string(),
            dial_target: "10.0.0.1".to_string(),
            protocol: "tcp".to_string(),
            port_from: port,
            port_to: port,
        }
    }

    // ── dial_target (Sprint 16 Phase 8.1) ────────────────────────────────────
    //
    // These sit directly on a non-negotiable project rule: a shield may only ever
    // reach 127.0.0.1 or its own LAN IP. `local_target` was added to SELECT from
    // that set, and the danger of such a change is that it quietly WIDENS it.
    // Nothing in the type system prevents that, so it is pinned here.
    //
    // detect_lan_ip() reads real interfaces, so the LAN-IP arm cannot be forced
    // from a unit test. Every assertion below is therefore written against values
    // whose verdict does not depend on this machine's addresses: loopback (always
    // allowed) and TEST-NET-3 / a hostname (never allowed anywhere).

    /// 203.0.113.0/24 is TEST-NET-3 (RFC 5737) — reserved for documentation and
    /// guaranteed never to be a real interface address, so "rejected" is a fact
    /// about the code rather than about the machine running the test.
    const NOT_OURS: &str = "203.0.113.7";

    #[test]
    fn empty_local_target_falls_back_to_host() {
        // The pre-Phase-8 behaviour, and what an un-upgraded controller sends.
        assert_eq!(dial_target("127.0.0.1", "").unwrap(), "127.0.0.1");
    }

    #[test]
    fn whitespace_local_target_is_treated_as_absent() {
        // A hand-inserted DB row bypasses the controller's blankToNil, so " "
        // must not read as "the operator asked for an address".
        assert_eq!(dial_target("127.0.0.1", "   ").unwrap(), "127.0.0.1");
    }

    #[test]
    fn loopback_local_target_is_accepted_and_selected() {
        // The whole point of the phase: a loopback-only service becomes dialable.
        assert_eq!(dial_target("127.0.0.1", "127.0.0.1").unwrap(), "127.0.0.1");
    }

    #[test]
    fn local_target_outside_the_allowed_set_is_rejected() {
        let err = dial_target("127.0.0.1", NOT_OURS).unwrap_err();
        assert_eq!(err.field, "local_target");
        assert_eq!(err.value, NOT_OURS);
    }

    /// A hostname must never be accepted here. Resolving names inside the shield
    /// is the shield-as-segment-gateway design the sprint explicitly defers; a
    /// tolerant parse would smuggle a resolver in through this field.
    #[test]
    fn hostname_local_target_is_rejected() {
        let err = dial_target("127.0.0.1", "db.internal").unwrap_err();
        assert_eq!(err.field, "local_target");
        assert_eq!(err.value, "db.internal");
    }

    /// The invariant no compiler protects: `host` binds a resource to THIS shield,
    /// so it is validated even when `local_target` overrides the dial address.
    /// Without this, an instruction naming ANOTHER shield's LAN IP would be applied
    /// here as long as it carried `local_target: "127.0.0.1"`.
    #[test]
    fn host_is_still_validated_when_local_target_is_set() {
        let err = dial_target(NOT_OURS, "127.0.0.1").unwrap_err();
        assert_eq!(
            err.field, "host",
            "a resource bound to another shield must be refused on `host`, \
             regardless of a valid local_target"
        );
        assert_eq!(err.value, NOT_OURS);
    }

    #[test]
    fn unaddressable_resource_is_rejected_on_host() {
        // migration 030 made `host` nullable and the desired-set query COALESCEs it
        // to "", so an empty host is reachable here. Fail closed on it.
        let err = dial_target("", "").unwrap_err();
        assert_eq!(err.field, "host");
    }

    /// The rejection must name the field, because the two failure modes send an
    /// operator to different columns. Before Phase 8 a local_target typo reported
    /// "resource host does not match this shield's LAN IP" — the wrong field.
    #[test]
    fn rejection_names_the_field_that_failed() {
        assert_eq!(dial_target(NOT_OURS, "").unwrap_err().field, "host");
        assert_eq!(
            dial_target("127.0.0.1", NOT_OURS).unwrap_err().field,
            "local_target"
        );
    }

    /// Whatever this machine's LAN IP is, it must be accepted as both `host` and
    /// `local_target` — that is the "selects from the set, does not extend it"
    /// property from the other side. Skipped where no LAN IP is detectable (CI
    /// containers), since there is then nothing to assert about.
    #[test]
    fn own_lan_ip_is_accepted_as_either_field() {
        let Some(my_ip) = util::detect_lan_ip() else {
            eprintln!("no LAN IP detected — skipping");
            return;
        };
        assert_eq!(dial_target(&my_ip, "").unwrap(), my_ip);
        assert_eq!(dial_target(&my_ip, &my_ip).unwrap(), my_ip);
        // And the cross case the phase file calls out: LAN-IP host, loopback target.
        assert_eq!(dial_target(&my_ip, "127.0.0.1").unwrap(), "127.0.0.1");
    }

    // ── dial_target_for (Sprint 16 Phase 8.2) ────────────────────────────────
    //
    // The tunnel path's only source of a dial address. If this ever returned
    // something derived from the TunnelOpen message rather than from applied state,
    // the connector would get free-form dialing inside the shield.

    fn state_with(resources: Vec<ActiveResource>) -> Arc<SharedResourceState> {
        let state = Arc::new(SharedResourceState::new());
        *state.active.lock().unwrap() = resources;
        state
    }

    #[test]
    fn dial_target_for_returns_the_stored_target_not_the_host() {
        let mut r = res("r1", 8080);
        r.dial_target = "127.0.0.1".to_string(); // loopback-only service
        let state = state_with(vec![r]);
        assert_eq!(
            state.dial_target_for("r1").as_deref(),
            Some("127.0.0.1"),
            "must return the validated dial_target, not host (10.0.0.1)"
        );
    }

    #[test]
    fn dial_target_for_is_none_for_an_unapplied_resource() {
        let state = state_with(vec![res("r1", 8080)]);
        assert!(state.dial_target_for("r-unknown").is_none());
        assert!(state.dial_target_for("").is_none());
    }

    #[test]
    fn dial_target_for_selects_the_matching_resource() {
        let mut a = res("r1", 8080);
        a.dial_target = "127.0.0.1".to_string();
        let b = res("r2", 9090); // dial_target stays 10.0.0.1
        let state = state_with(vec![a, b]);
        assert_eq!(state.dial_target_for("r1").as_deref(), Some("127.0.0.1"));
        assert_eq!(state.dial_target_for("r2").as_deref(), Some("10.0.0.1"));
    }

    // F21: the rebuild must flush the chain BEFORE adding the new rules, in one batch.
    // Flushing AFTER the adds would wipe the new rules → empty chain → policy Accept →
    // every protected port wide open (fail-open). This guards that ordering.
    #[test]
    fn flush_precedes_rule_adds() {
        let ruleset = build_protect_ruleset(&[res("r1", 8080)]);
        let objs = ruleset.objects.as_ref();

        let flush_idx = objs
            .iter()
            .position(|o| matches!(o, NfObject::CmdObject(NfCmd::Flush(FlushObject::Chain(_)))))
            .expect("rebuild must flush the protect chain");
        let first_rule_idx = objs
            .iter()
            .position(|o| matches!(o, NfObject::CmdObject(NfCmd::Add(NfListObject::Rule(_)))))
            .expect("rebuild must add rules");

        assert!(
            flush_idx < first_rule_idx,
            "flush (idx {flush_idx}) must precede rule adds (idx {first_rule_idx}); \
             flushing after would wipe the new rules and fail open"
        );
    }

    // Sanity: the whole rebuild is ONE Nftables transaction (atomic swap on commit),
    // not multiple batches. table + chain + flush + 3 rules (tcp) = 6 commands.
    #[test]
    fn rebuild_is_single_transaction() {
        let ruleset = build_protect_ruleset(&[res("r1", 8080)]);
        let objs = ruleset.objects.as_ref();
        assert_eq!(
            objs.len(),
            6,
            "expected table+chain+flush+3 rules in one batch"
        );
        let rule_adds = objs
            .iter()
            .filter(|o| matches!(o, NfObject::CmdObject(NfCmd::Add(NfListObject::Rule(_)))))
            .count();
        assert_eq!(
            rule_adds, 3,
            "expected 3 rules: lo-accept, localhost-accept, port-drop"
        );
    }

    // ---- F21 live-kernel verification (gated) -------------------------------
    // These apply real rulesets to the running kernel, so they need root +
    // CAP_NET_ADMIN and must NOT touch a real host firewall. Run them inside a
    // throwaway rootless network namespace, where you are root and nftables is
    // fully isolated from the host:
    //
    //   cargo test -p zecurity-shield --lib resources::tests::live_nft --no-run
    //   unshare -rn <the compiled test binary> live_nft --ignored --nocapture
    //
    // (or simply: unshare -rn cargo test -p zecurity-shield --lib live_nft --ignored --nocapture)
    // They are #[ignore] so normal `cargo test` / CI skips them.

    /// True iff the protect chain currently exists AND carries its port-drop
    /// rule. The chain's only `drop` statement is the port-drop rule (policy is
    /// `accept`), so "chain lists OK and contains drop" == "the port is defended".
    /// A flushed/missing chain → no `drop` → returns false.
    fn chain_drop_present() -> bool {
        match std::process::Command::new("nft")
            .args(["list", "chain", "inet", TABLE, PROTECT_CHAIN])
            .output()
        {
            Ok(o) => o.status.success() && String::from_utf8_lossy(&o.stdout).contains("drop"),
            Err(_) => false,
        }
    }

    // F21 (a): no enforcement gap during a rebuild. A concurrent observer hammers
    // `nft list chain` while we re-apply (flush+rebuild) 50 times. Because each
    // apply is ONE atomic `nft -f` batch, the observer must only ever see the old
    // or the new complete ruleset — never an empty/undefended chain mid-swap.
    #[test]
    #[ignore = "live kernel test; run inside 'unshare -rn' (see module comment)"]
    fn live_nft_rebuild_no_gap() {
        use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
        use std::sync::Arc;

        let resources = vec![res("r1", 8080)];

        // Reach steady state first so the sampler never races initial chain creation.
        helper::apply_ruleset(&build_protect_ruleset(&resources)).expect("initial apply");
        assert!(
            chain_drop_present(),
            "drop rule must be present after the initial apply"
        );

        let stop = Arc::new(AtomicBool::new(false));
        let samples = Arc::new(AtomicU64::new(0));
        let gaps = Arc::new(AtomicU64::new(0));
        let (s, g, stp) = (samples.clone(), gaps.clone(), stop.clone());

        let sampler = std::thread::spawn(move || {
            while !stp.load(Ordering::Relaxed) {
                s.fetch_add(1, Ordering::Relaxed);
                if !chain_drop_present() {
                    g.fetch_add(1, Ordering::Relaxed);
                }
            }
        });

        for _ in 0..50 {
            helper::apply_ruleset(&build_protect_ruleset(&resources)).expect("rebuild apply");
        }

        stop.store(true, Ordering::Relaxed);
        sampler.join().unwrap();

        let total = samples.load(Ordering::Relaxed);
        let gap = gaps.load(Ordering::Relaxed);
        eprintln!("[F21 no-gap] {total} samples across 50 rebuilds; {gap} saw the port undefended");
        assert!(total > 0, "sampler took no samples");
        assert_eq!(
            gap, 0,
            "port observed undefended {gap}/{total} times during rebuild — fail-open gap"
        );
    }

    // F21 (b): a failed apply leaves the old rules intact (no fail-open). We inject
    // a single transaction that flushes the chain and then adds a rule the kernel
    // rejects (jump to a nonexistent chain) — the exact `flush chain; add rule …`
    // shape build_protect_ruleset emits, but with a failing add. Because nft applies
    // `-f` atomically, the flush MUST roll back with the failed add, so the prior
    // ruleset survives. This is the worse half of the original F21 bug.
    #[test]
    #[ignore = "live kernel test; run inside 'unshare -rn' (see module comment)"]
    fn live_nft_failed_apply_preserves_rules() {
        use std::io::Write;
        use std::process::{Command, Stdio};

        let resources = vec![res("r1", 8080)];
        helper::apply_ruleset(&build_protect_ruleset(&resources)).expect("good apply");
        assert!(
            chain_drop_present(),
            "drop rule must be present after the good apply"
        );

        let script = format!(
            "flush chain inet {TABLE} {PROTECT_CHAIN}\n\
             add rule inet {TABLE} {PROTECT_CHAIN} tcp dport 8080 jump nonexistent_chain_zzz\n"
        );
        let mut child = Command::new("nft")
            .args(["-f", "-"])
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .expect("spawn nft");
        child
            .stdin
            .take()
            .unwrap()
            .write_all(script.as_bytes())
            .unwrap();
        let out = child.wait_with_output().unwrap();
        assert!(
            !out.status.success(),
            "injected apply should fail (jump to nonexistent chain); nft said: {}",
            String::from_utf8_lossy(&out.stderr)
        );

        assert!(
            chain_drop_present(),
            "old drop rule must survive a failed apply — the flush has to roll back too (F21 no fail-open)"
        );
    }

    // ---- Sprint 16 Phase 8 live verification (gated) ------------------------
    //
    // The phase's headline claim — "a loopback-only service becomes reachable" —
    // cannot be shown by a pure unit test: it needs real nftables, a real socket,
    // and a LAN IP that is NOT the address the service is bound to. All three are
    // reproducible inside the same throwaway namespace the F21 tests use, given a
    // dummy interface carrying an RFC-1918 address ("dummy" is not in
    // util::VIRTUAL_PREFIXES, so detect_lan_ip() picks it up):
    //
    //   unshare -rn sh -c '
    //     ip link set lo up
    //     ip link add dummy0 type dummy
    //     ip addr add 10.99.0.1/24 dev dummy0
    //     ip link set dummy0 up
    //     cargo test -p zecurity-shield --lib live_phase8 -- --ignored --nocapture'
    //
    // Without the dummy interface detect_lan_ip() returns None inside the netns and
    // these skip themselves rather than asserting something meaningless.

    /// Bind a listener on 127.0.0.1 ONLY — the hardened posture the phase exists
    /// for. Returns the port; the listener stays alive via the returned handle.
    fn loopback_only_listener() -> (std::net::TcpListener, u16) {
        let l = std::net::TcpListener::bind("127.0.0.1:0").expect("bind loopback");
        let port = l.local_addr().unwrap().port();
        (l, port)
    }

    fn instruction(host: &str, local_target: &str, port: u16) -> crate::proto::ResourceInstruction {
        crate::proto::ResourceInstruction {
            resource_id: "res-phase8".to_string(),
            host: host.to_string(),
            protocol: "tcp".to_string(),
            port_from: port as i32,
            port_to: port as i32,
            action: "apply".to_string(),
            local_target: local_target.to_string(),
        }
    }

    /// THE phase 8 acceptance test, both halves in one run so the contrast is the
    /// assertion:
    ///
    ///   without local_target → the shield probes its LAN IP, the loopback-only
    ///                          service is unreachable, ack = "failed"
    ///   with    local_target → the shield probes 127.0.0.1, ack = "protected",
    ///                          and a real TCP connection to the stored dial target
    ///                          reaches the service
    ///
    /// The first half is not incidental — it is the bug being fixed, demonstrated.
    #[tokio::test]
    #[ignore = "live kernel test; run inside 'unshare -rn' with a dummy LAN IP (see module comment)"]
    async fn live_phase8_loopback_only_service_becomes_reachable() {
        let Some(lan_ip) = util::detect_lan_ip() else {
            eprintln!("no RFC-1918 LAN IP in this namespace — add dummy0 (see module comment)");
            return;
        };
        assert_ne!(
            lan_ip, "127.0.0.1",
            "the LAN IP must differ from loopback or this test proves nothing"
        );

        let (_listener, port) = loopback_only_listener();
        eprintln!("[phase8] lan_ip={lan_ip} loopback-only service on 127.0.0.1:{port}");

        // ── Half 1: no local_target — the pre-Phase-8 behaviour ──
        let state = Arc::new(SharedResourceState::new());
        let ack = handle_apply(&instruction(&lan_ip, "", port), &state).await;
        eprintln!(
            "[phase8] without local_target → status={} port_reachable={}",
            ack.status, ack.port_reachable
        );
        assert_eq!(
            ack.status, "failed",
            "a loopback-only service probed at the LAN IP must be unreachable — \
             if this says 'protected', the test is not proving anything"
        );
        assert!(!ack.port_reachable);

        // ── Half 2: local_target = loopback ──
        let state = Arc::new(SharedResourceState::new());
        let ack = handle_apply(&instruction(&lan_ip, "127.0.0.1", port), &state).await;
        eprintln!(
            "[phase8] with local_target=127.0.0.1 → status={} port_reachable={}",
            ack.status, ack.port_reachable
        );
        assert_eq!(
            ack.status, "protected",
            "with local_target the shield must probe loopback and find the service"
        );
        assert!(ack.port_reachable);

        // The stored target is what the tunnel path will dial.
        let target = state
            .dial_target_for("res-phase8")
            .expect("resource must be in the applied set");
        assert_eq!(
            target, "127.0.0.1",
            "tunnel must dial loopback, not the LAN IP"
        );

        // And it genuinely connects — the end of the chain this phase exists to close.
        std::net::TcpStream::connect(format!("{target}:{port}"))
            .expect("a real TCP connection to the stored dial target must reach the service");
        eprintln!("[phase8] TCP connect to {target}:{port} OK — loopback-only service reachable");
    }

    /// The rejection half of 8.1 against the live path: an address outside the
    /// allowed set must ack "failed" and must NOT enter the applied set, so the
    /// tunnel path has nothing to dial for it.
    #[tokio::test]
    #[ignore = "live kernel test; run inside 'unshare -rn' with a dummy LAN IP (see module comment)"]
    async fn live_phase8_foreign_local_target_is_refused() {
        let Some(lan_ip) = util::detect_lan_ip() else {
            eprintln!("no RFC-1918 LAN IP in this namespace — add dummy0 (see module comment)");
            return;
        };
        let (_listener, port) = loopback_only_listener();
        let state = Arc::new(SharedResourceState::new());

        let ack = handle_apply(&instruction(&lan_ip, NOT_OURS, port), &state).await;
        assert_eq!(ack.status, "failed");
        assert!(
            ack.error.contains("local_target"),
            "the ack must name the offending field, got: {}",
            ack.error
        );
        assert!(
            state.dial_target_for("res-phase8").is_none(),
            "a refused resource must never enter the applied set"
        );

        // A hostname is refused the same way — the shield is not a resolver.
        let ack = handle_apply(&instruction(&lan_ip, "db.internal", port), &state).await;
        assert_eq!(ack.status, "failed");
        assert!(state.dial_target_for("res-phase8").is_none());
    }
}
