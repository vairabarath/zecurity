use std::borrow::Cow;
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use nftables::{
    batch::Batch,
    expr::{Expression, Meta, MetaKey, NamedExpression, Payload, PayloadField, Range},
    helper,
    schema::{Chain, NfListObject, NfObject, Rule},
    stmt::{Accept, Drop, Match, Operator, Statement},
    types::{NfChainPolicy, NfChainType, NfFamily, NfHook},
};
use tracing::info;

use crate::proto::ResourceAck;
use crate::{appmeta, util};

const TABLE: &str = "zecurity";
const PROTECT_CHAIN: &str = "resource_protect";

#[derive(Clone)]
pub struct ActiveResource {
    pub resource_id: String,
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

pub fn check_port(port: u16) -> bool {
    std::net::TcpStream::connect_timeout(
        &format!("127.0.0.1:{}", port).parse().unwrap(),
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
            // Allow loopback and zecurity0 (ZTNA tunnel); drop everything else.
            batch.add(NfListObject::Rule(iif_accept_rule(proto, port_expr.clone(), "lo")));
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

    info!(resource_count = resources.len(), "rebuilt nftables resource_protect chain");
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
                let reachable = check_port(res.port_from);
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
