use std::borrow::Cow;
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use nftables::{
    batch::Batch,
    expr::{Expression, Meta, MetaKey, NamedExpression, Payload, PayloadField, Range},
    helper,
    schema::{Chain, NfListObject, NfObject, Rule, Table},
    stmt::{Accept, Drop, Match, Operator, Statement},
    types::{NfChainPolicy, NfChainType, NfFamily, NfHook},
};
use tracing::{error, info, warn};

use crate::proto::ResourceAck;
use crate::{appmeta, util};

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
}

impl SharedResourceState {
    pub fn new() -> Self {
        Self {
            active: Mutex::new(Vec::new()),
            acks: Mutex::new(Vec::new()),
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
    std::net::TcpStream::connect_timeout(
        &format!("{}:{}", host, port).parse().unwrap(),
        Duration::from_secs(2),
    )
    .is_ok()
}

async fn chain_exists() -> Result<bool> {
    let ruleset = helper::get_current_ruleset_async()
        .await
        .context("failed to query nftables ruleset")?;

    Ok(ruleset.objects.iter().any(|obj| {
        matches!(
            obj,
            NfObject::ListObject(NfListObject::Chain(Chain {
                family: NfFamily::INet,
                table,
                name,
                ..
            })) if table.as_ref() == TABLE && name.as_ref() == PROTECT_CHAIN
        )
    }))
}

/// Flush and atomically rebuild `chain resource_protect` for the given resource list.
pub async fn apply_nftables(resources: &[ActiveResource]) -> Result<()> {
    if chain_exists().await? {
        let mut del = Batch::new();
        del.delete(NfListObject::Chain(Chain {
            family: NfFamily::INet,
            table: TABLE.into(),
            name: PROTECT_CHAIN.into(),
            ..Chain::default()
        }));
        helper::apply_ruleset_async(&del.to_nftables())
            .await
            .context("failed to flush resource_protect chain")?;
    }

    let mut batch = Batch::new();
    // Ensure the parent table exists. `add table` is idempotent — no-op if
    // network::setup() already created it, but recovers if setup() failed.
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

    for res in resources {
        let protos: &[&str] = match res.protocol.as_str() {
            "tcp" => &["tcp"],
            "udp" => &["udp"],
            _ => &["tcp", "udp"],
        };
        for proto in protos {
            let port_expr = port_expression(res.port_from, res.port_to);
            // Allow loopback and zecurity0 (ZTNA tunnel); drop everything else for this port.
            batch.add(NfListObject::Rule(iif_accept_rule(
                proto,
                port_expr.clone(),
                "lo",
            )));
            batch.add(NfListObject::Rule(iif_accept_rule(
                proto,
                port_expr.clone(),
                appmeta::SHIELD_INTERFACE_NAME,
            )));
            batch.add(NfListObject::Rule(port_drop_rule(proto, port_expr)));
        }
    }

    helper::apply_ruleset_async(&batch.to_nftables())
        .await
        .context("failed to apply resource_protect chain")?;

    info!(
        resource_count = resources.len(),
        "rebuilt nftables resource_protect chain"
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
