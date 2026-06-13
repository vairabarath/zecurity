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

pub fn check_port(host: &str, port: u16) -> bool {
    // Hosts reaching here are validated IPs (127.0.0.1 or detect_lan_ip()), so this
    // parses in practice — but never panic on a malformed address: an unparseable
    // host is simply treated as not-listening (fail to `failed`, not a shield crash).
    let addr = match format!("{}:{}", host, port).parse::<std::net::SocketAddr>() {
        Ok(addr) => addr,
        Err(_) => {
            warn!(host = host, port = port, "check_port: unparseable address, treating as unreachable");
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
                let reachable = check_port(&res.host, res.port_from);
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

    if !validate_host(&instruction.host) {
        warn!(
            resource_id = %instruction.resource_id,
            host = %instruction.host,
            "resource host does not match this shield's LAN IP — rejecting"
        );
        return ResourceAck {
            resource_id: instruction.resource_id.clone(),
            status: "failed".to_string(),
            error: "resource host does not match this shield's IP".to_string(),
            verified_at: now,
            port_reachable: false,
        };
    }

    {
        let mut active = state.active.lock().unwrap();
        if let Some(existing) = active
            .iter_mut()
            .find(|r| r.resource_id == instruction.resource_id)
        {
            existing.host = instruction.host.clone();
            existing.protocol = instruction.protocol.clone();
            existing.port_from = instruction.port_from as u16;
            existing.port_to = instruction.port_to as u16;
        } else {
            active.push(ActiveResource {
                resource_id: instruction.resource_id.clone(),
                host: instruction.host.clone(),
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
            let reachable = check_port(&instruction.host, instruction.port_from as u16);
            info!(
                resource_id = %instruction.resource_id,
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
        if !validate_host(&res.host) {
            warn!(
                resource_id = %res.resource_id,
                host = %res.host,
                "snapshot resource host does not match this shield's LAN IP — skipping"
            );
            acks.push(ResourceAck {
                resource_id: res.resource_id.clone(),
                status: "failed".to_string(),
                error: "resource host does not match this shield's IP".to_string(),
                verified_at: now,
                port_reachable: false,
            });
            continue;
        }
        new_active.push(ActiveResource {
            resource_id: res.resource_id.clone(),
            host: res.host.clone(),
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
                let reachable = check_port(&r.host, r.port_from);
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
                warn!(source = source, "invalid prefix length in source rule, using literal");
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
            protocol: "tcp".to_string(),
            port_from: port,
            port_to: port,
        }
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
        assert_eq!(objs.len(), 6, "expected table+chain+flush+3 rules in one batch");
        let rule_adds = objs
            .iter()
            .filter(|o| matches!(o, NfObject::CmdObject(NfCmd::Add(NfListObject::Rule(_)))))
            .count();
        assert_eq!(rule_adds, 3, "expected 3 rules: lo-accept, localhost-accept, port-drop");
    }
}
