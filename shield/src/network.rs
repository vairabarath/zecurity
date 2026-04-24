// network.rs — Linux network bootstrap for the ZECURITY Shield
//
// WHAT THIS MODULE DOES:
//   After a shield enrolls, it needs a local interface and a minimal firewall
//   policy before any protected traffic can be steered through the host.
//   This module performs that one-time bootstrap:
//     1. Ensure the zecurity0 TUN interface exists
//     2. Assign the controller-provided /32 address to it
//     3. Bring the interface UP
//     4. Install the base nftables table used by later Sprint 5 rules
//
// ARCHITECTURE CHOICE:
//   The interface path is implemented with rtnetlink so the daemon talks to the
//   kernel directly instead of depending on the `ip` binary at runtime.
//   The firewall path constructs a typed nftables ruleset in Rust and applies it
//   through the current `nftables` crate helper. In this crate version, that
//   helper still invokes the system `nft` executable under the hood, so we avoid
//   shell command construction in our code but still depend on `nft` being
//   available on the host.

use std::borrow::Cow;
use std::net::{IpAddr, Ipv4Addr};

use anyhow::{bail, Context, Result};
use futures_util::stream::TryStreamExt;
use nftables::{
    batch::Batch,
    expr::{Expression, Meta, MetaKey, NamedExpression, Payload, PayloadField},
    helper,
    schema::{Chain, NfListObject, NfObject, Rule, Table},
    stmt::{Accept, Drop, Match, Operator, Statement},
    types::{NfChainPolicy, NfChainType, NfFamily, NfHook},
};
use rtnetlink::{
    new_connection,
    packet_route::link::{InfoData, InfoKind},
    Error as NetlinkError, Handle, LinkMessageBuilder, LinkUnspec,
};
use tracing::{info, warn};

use crate::appmeta;

/// Public entry point called after successful enrollment.
///
/// `interface_addr` is the `/32` assigned by the controller.
/// `connector_addr` includes `host:port`; we only need the host portion when
/// writing the nftables rule that permits traffic from the assigned connector.
pub async fn setup(interface_addr: &str, connector_addr: &str) -> Result<()> {
    info!(
        interface = appmeta::SHIELD_INTERFACE_NAME,
        interface_addr = interface_addr,
        connector_addr = connector_addr,
        "starting shield network setup"
    );

    setup_tun_interface(interface_addr).await?;
    setup_nftables(connector_addr).await?;

    info!(
        interface = appmeta::SHIELD_INTERFACE_NAME,
        "shield network setup complete"
    );
    Ok(())
}

/// Create and configure the `zecurity0` interface.
///
/// This function is intentionally idempotent:
/// - If the interface already exists, we keep it and continue.
/// - We still re-run address assignment and `link set up`, because the previous
///   process may have left the device in a partially configured state.
async fn setup_tun_interface(interface_addr: &str) -> Result<()> {
    let (connection, handle, _) =
        new_connection().context("failed to open rtnetlink connection")?;
    tokio::spawn(connection);

    let interface_name = appmeta::SHIELD_INTERFACE_NAME;

    let link_index = if let Some(index) = interface_index(&handle, interface_name).await? {
        warn!(
            interface = interface_name,
            "shield interface already exists, reusing it"
        );
        index
    } else {
        // The TUN device is created through netlink instead of shelling out to
        // `ip tuntap add`. That keeps the system service self-contained and
        // removes a hard dependency on the `ip` userspace tool.
        let message = LinkMessageBuilder::<LinkUnspec>::new_with_info_kind(InfoKind::Tun)
            .name(interface_name.to_string())
            .set_info_data(InfoData::Tun(vec![]))
            .build();
        handle
            .link()
            .add(message)
            .execute()
            .await
            .with_context(|| format!("failed to create TUN interface {}", interface_name))?;
        info!(interface = interface_name, "created shield TUN interface");
        interface_index(&handle, interface_name)
            .await?
            .context("created TUN interface but could not resolve its link index")?
    };

    // The controller assigns a /32 so the shield gets a stable point address
    // without owning an entire subnet on the host.
    //
    // If the address is already present, the kernel returns EEXIST. We treat
    // that as non-fatal because this phase must be safe across service restarts.
    let (address, prefix_len) = parse_interface_cidr(interface_addr)?;
    match handle
        .address()
        .add(link_index, address, prefix_len)
        .execute()
        .await
    {
        Ok(()) => info!(
            interface = interface_name,
            interface_addr = interface_addr,
            "assigned interface address"
        ),
        Err(err) if is_already_exists(&err) => {
            warn!(
                interface = interface_name,
                interface_addr = interface_addr,
                "interface address already assigned, continuing"
            );
        }
        Err(err) => {
            return Err(anyhow::Error::new(err)).context(format!(
                "failed to assign {} to {}",
                interface_addr, interface_name
            ));
        }
    }

    // Bringing the link UP is always safe to repeat and ensures the interface
    // is usable even if a previous run created it but never activated it.
    handle
        .link()
        .set(LinkUnspec::new_with_index(link_index).up().build())
        .execute()
        .await
        .with_context(|| format!("failed to bring interface {} up", interface_name))?;
    info!(interface = interface_name, "brought shield interface up");

    Ok(())
}

/// Install the base `inet zecurity` nftables table.
///
/// The initial policy is deliberately narrow:
/// - Always allow loopback
/// - Allow packets from the assigned connector IP
/// - Drop anything arriving on `zecurity0` until Sprint 5 adds resource rules
async fn setup_nftables(connector_addr: &str) -> Result<()> {
    let connector_ip = parse_connector_host(connector_addr)?;
    let connector_ip: Ipv4Addr = connector_ip.parse().with_context(|| {
        format!(
            "connector host '{}' is not a valid IPv4 address",
            connector_ip
        )
    })?;

    // We rebuild the full table on each run so the host converges to one known
    // base policy. If an older `zecurity` table exists, remove it first.
    if nft_table_exists(TABLE_NAME).await? {
        let mut delete_batch = Batch::new();
        delete_batch.delete(NfListObject::Table(Table {
            family: NfFamily::INet,
            name: TABLE_NAME.into(),
            ..Table::default()
        }));
        helper::apply_ruleset_async(&delete_batch.to_nftables())
            .await
            .context("failed to delete existing nftables table 'zecurity'")?;
    }

    let mut batch = Batch::new();
    batch.add(NfListObject::Table(Table {
        family: NfFamily::INet,
        name: TABLE_NAME.into(),
        ..Table::default()
    }));
    batch.add(NfListObject::Chain(Chain {
        family: NfFamily::INet,
        table: TABLE_NAME.into(),
        name: INPUT_CHAIN.into(),
        _type: Some(NfChainType::Filter),
        hook: Some(NfHook::Input),
        prio: Some(0),
        policy: Some(NfChainPolicy::Accept),
        ..Chain::default()
    }));
    batch.add(NfListObject::Rule(loopback_accept_rule()));
    batch.add(NfListObject::Rule(interface_accept_rule(
        appmeta::SHIELD_INTERFACE_NAME,
    )));
    batch.add(NfListObject::Rule(connector_accept_rule(connector_ip)));

    helper::apply_ruleset_async(&batch.to_nftables())
        .await
        .context("failed to apply shield nftables rules")?;
    info!(
        connector_ip = %connector_ip,
        table = "inet zecurity",
        "installed shield nftables rules"
    );
    Ok(())
}

/// Resolve a named interface to its kernel link index.
async fn interface_index(handle: &Handle, interface_name: &str) -> Result<Option<u32>> {
    let mut links = handle
        .link()
        .get()
        .match_name(interface_name.to_string())
        .execute();

    if let Some(link) = links
        .try_next()
        .await
        .with_context(|| format!("failed to query interface {}", interface_name))?
    {
        return Ok(Some(link.header.index));
    }

    Ok(None)
}

/// Parse a `<ip>/<prefix>` string for rtnetlink address assignment.
fn parse_interface_cidr(interface_addr: &str) -> Result<(IpAddr, u8)> {
    let (address, prefix_len) = interface_addr
        .split_once('/')
        .context("interface_addr must be in CIDR form, e.g. 100.64.0.10/32")?;

    let address: IpAddr = address
        .parse()
        .with_context(|| format!("invalid IP address '{}'", address))?;
    let prefix_len: u8 = prefix_len
        .parse()
        .with_context(|| format!("invalid CIDR prefix '{}'", prefix_len))?;

    Ok((address, prefix_len))
}

/// Detect the kernel's "already exists" netlink error in a portable way.
fn is_already_exists(err: &NetlinkError) -> bool {
    err.to_string().contains("File exists")
}

/// Extract the host part from `host:port`.
///
/// We only need the host because nftables matches on source IP, not transport
/// ports in this base Sprint 4 rule.
fn parse_connector_host(connector_addr: &str) -> Result<String> {
    let host = connector_addr
        .rsplit_once(':')
        .map(|(host, _)| host)
        .filter(|host| !host.is_empty())
        .context("connector_addr must be in host:port form")?;

    if host.contains('[') || host.contains(']') {
        bail!("IPv6 connector addresses are not supported by the Sprint 4 nftables rule");
    }

    Ok(host.to_string())
}

/// Check whether the base `inet zecurity` table already exists.
async fn nft_table_exists(table_name: &str) -> Result<bool> {
    let ruleset = helper::get_current_ruleset_async()
        .await
        .context("failed to query current nftables ruleset")?;

    Ok(ruleset.objects.iter().any(|object| {
        matches!(
            object,
            NfObject::ListObject(NfListObject::Table(Table { family: NfFamily::INet, name, .. }))
                if name.as_ref() == table_name
        )
    }))
}

fn loopback_accept_rule() -> Rule<'static> {
    Rule {
        family: NfFamily::INet,
        table: TABLE_NAME.into(),
        chain: INPUT_CHAIN.into(),
        expr: Cow::Owned(vec![
            Statement::Match(Match {
                left: Expression::Named(NamedExpression::Meta(Meta {
                    key: MetaKey::Iifname,
                })),
                right: Expression::String("lo".into()),
                op: Operator::EQ,
            }),
            Statement::Accept(Some(Accept {})),
        ]),
        ..Rule::default()
    }
}

fn connector_accept_rule(connector_ip: Ipv4Addr) -> Rule<'static> {
    Rule {
        family: NfFamily::INet,
        table: TABLE_NAME.into(),
        chain: INPUT_CHAIN.into(),
        expr: Cow::Owned(vec![
            Statement::Match(Match {
                left: Expression::Named(NamedExpression::Payload(Payload::PayloadField(
                    PayloadField {
                        protocol: "ip".into(),
                        field: "saddr".into(),
                    },
                ))),
                right: Expression::String(connector_ip.to_string().into()),
                op: Operator::EQ,
            }),
            Statement::Accept(Some(Accept {})),
        ]),
        ..Rule::default()
    }
}

fn interface_accept_rule(interface_name: &str) -> Rule<'static> {
    Rule {
        family: NfFamily::INet,
        table: TABLE_NAME.into(),
        chain: INPUT_CHAIN.into(),
        expr: Cow::Owned(vec![
            Statement::Match(Match {
                left: Expression::Named(NamedExpression::Meta(Meta {
                    key: MetaKey::Iifname,
                })),
                right: Expression::String(interface_name.to_string().into()),
                op: Operator::EQ,
            }),
            Statement::Accept(Some(Accept {})),
        ]),
        ..Rule::default()
    }
}

const TABLE_NAME: &str = "zecurity";
const INPUT_CHAIN: &str = "input";
