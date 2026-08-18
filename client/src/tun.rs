use std::collections::BTreeSet;
use std::net::Ipv4Addr;
use std::process::{Command, Stdio};

use anyhow::{Context, Result};
use futures::TryStreamExt;
use rtnetlink::Handle;

use crate::registry::Net;

const ZECURITY_TABLE: &str = "zecurity_client";
const ZECURITY_CHAIN: &str = "output";
const ZECURITY_MARK: &str = "0x5a";
const ZECURITY_ROUTE_TABLE: &str = "105";
const ZECURITY_RULE_PRIORITY: &str = "49";

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct AllowedFlow {
    pub ip: Ipv4Addr,
    pub port: u16,
}

pub struct TunManager {
    dev: Option<tun::AsyncDevice>,
    policy_ips: Vec<Ipv4Addr>,
    if_index: u32,
    handle: Handle,
}

impl TunManager {
    pub async fn create() -> Result<Self> {
        cleanup_stale_interface().await;

        let mut config = tun::Configuration::default();
        config
            .name("zecurity0")
            .address("100.64.0.1")
            .netmask("255.255.255.255")
            .up();

        let dev = tun::create_as_async(&config).context("create TUN device zecurity0")?;

        let (conn, handle, _) = rtnetlink::new_connection().context("open rtnetlink")?;
        tokio::spawn(conn);

        let if_index = if_index_by_name(&handle, "zecurity0")
            .await
            .context("get zecurity0 interface index")?;

        Ok(Self {
            dev: Some(dev),
            policy_ips: Vec::new(),
            if_index,
            handle,
        })
    }

    /// Route explicitly allowed TCP destination flows into zecurity0.
    ///
    /// nft marks matching local outbound packets before route lookup. The fwmark
    /// rule then selects table 105, where only the allowed destinations point at
    /// zecurity0. Other ports on the same IP remain unmarked and use the normal
    /// kernel route.
    ///
    /// `synthetic_cidr` (Sprint 16 Phase 9.3) adds ONE port-agnostic rule for the
    /// whole synthetic range instead of one rule per name-addressed resource, so
    /// the ruleset stays constant-size as resources grow. Pinned-IP resources keep
    /// their per-`(ip, port)` rule and per-`/32` route unchanged.
    pub fn configure_allowed_flows(
        &mut self,
        flows: &[AllowedFlow],
        synthetic_cidr: Option<Net>,
    ) -> Result<()> {
        cleanup_policy_routes();
        // A workspace with ONLY name-addressed resources has no pinned flows but
        // still needs the CIDR rule and route. Returning early on `flows.is_empty()`
        // alone would install nothing and silently break every FQDN resource.
        if flows.is_empty() && synthetic_cidr.is_none() {
            return Ok(());
        }

        let unique_flows: BTreeSet<AllowedFlow> = flows.iter().copied().collect();
        let unique_ips: BTreeSet<Ipv4Addr> = unique_flows.iter().map(|flow| flow.ip).collect();

        run_command("nft", &["add", "table", "inet", ZECURITY_TABLE])?;
        run_command(
            "nft",
            &[
                "add",
                "chain",
                "inet",
                ZECURITY_TABLE,
                ZECURITY_CHAIN,
                "{ type route hook output priority mangle; policy accept; }",
            ],
        )?;

        for rule in nft_rule_plan(&unique_flows, synthetic_cidr) {
            let args: Vec<&str> = rule.iter().map(String::as_str).collect();
            run_command("nft", &args)?;
        }

        run_command(
            "ip",
            &[
                "rule",
                "add",
                "fwmark",
                ZECURITY_MARK,
                "lookup",
                ZECURITY_ROUTE_TABLE,
                "priority",
                ZECURITY_RULE_PRIORITY,
            ],
        )?;

        // One route for the whole synthetic range — the per-/32 growth this phase
        // exists to remove. Goes into table 105 like everything else, so the
        // existing `ip route flush table 105` in cleanup_policy_routes() already
        // tears it down; a leaked CGNAT route would blackhole traffic host-wide.
        if let Some(cidr) = synthetic_cidr {
            let prefix = cidr.to_string();
            run_command(
                "ip",
                &[
                    "route",
                    "replace",
                    &prefix,
                    "dev",
                    "zecurity0",
                    "table",
                    ZECURITY_ROUTE_TABLE,
                ],
            )?;
        }

        for ip in &unique_ips {
            let prefix = format!("{ip}/32");
            run_command(
                "ip",
                &[
                    "route",
                    "replace",
                    &prefix,
                    "dev",
                    "zecurity0",
                    "table",
                    ZECURITY_ROUTE_TABLE,
                ],
            )?;
        }

        self.policy_ips = unique_ips.into_iter().collect();
        Ok(())
    }

    /// Hand the AsyncDevice to the smoltcp net_stack, keeping the rest of the manager alive.
    pub fn take_device(&mut self) -> Option<tun::AsyncDevice> {
        self.dev.take()
    }

    /// Remove all routes installed in this session and drop the TUN device.
    pub async fn cleanup(mut self) -> Result<()> {
        cleanup_policy_routes();
        self.policy_ips.clear();
        drop(self.dev.take());
        let _ = del_link_by_index(&self.handle, self.if_index).await;
        Ok(())
    }
}

impl Drop for TunManager {
    fn drop(&mut self) {
        // Best-effort: routes are already cleaned up by cleanup() in the normal path.
        // If we get here without cleanup(), the TUN device is dropped but routes may linger
        // until the next up. Log nothing — we're in a destructor.
        drop(self.dev.take());
    }
}

async fn if_index_by_name(handle: &Handle, name: &str) -> Result<u32> {
    let mut links = handle.link().get().match_name(name.to_string()).execute();
    if let Some(msg) = links.try_next().await? {
        return Ok(msg.header.index);
    }
    anyhow::bail!("interface {} not found", name)
}

async fn cleanup_stale_interface() {
    let Ok((conn, handle, _)) = rtnetlink::new_connection() else {
        return;
    };
    tokio::spawn(conn);
    if let Ok(if_index) = if_index_by_name(&handle, "zecurity0").await {
        let _ = del_link_by_index(&handle, if_index).await;
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
    }
}

async fn del_link_by_index(handle: &Handle, if_index: u32) -> Result<()> {
    handle
        .link()
        .del(if_index)
        .execute()
        .await
        .with_context(|| format!("rtnetlink delete link index {}", if_index))
}

fn cleanup_policy_routes() {
    let _ = Command::new("nft")
        .args(["delete", "table", "inet", ZECURITY_TABLE])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status();
    let _ = Command::new("ip")
        .args([
            "rule",
            "del",
            "fwmark",
            ZECURITY_MARK,
            "lookup",
            ZECURITY_ROUTE_TABLE,
            "priority",
            ZECURITY_RULE_PRIORITY,
        ])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status();
    let _ = Command::new("ip")
        .args([
            "rule",
            "del",
            "fwmark",
            ZECURITY_MARK,
            "lookup",
            "main",
            "priority",
            ZECURITY_RULE_PRIORITY,
        ])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status();
    let _ = Command::new("ip")
        .args(["route", "flush", "table", ZECURITY_ROUTE_TABLE])
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status();
}

/// Build the ordered nft rule list for one chain rebuild.
///
/// Extracted as a pure function because the ORDER here is security-critical and
/// was otherwise only observable on a live kernel. The Gate 1 co-location loop —
/// which cost a day — was exactly a rule-ordering failure, and nothing in the
/// codebase asserted the ordering until now.
///
/// Each entry is a complete `nft` argument list.
fn nft_rule_plan(flows: &BTreeSet<AllowedFlow>, synthetic_cidr: Option<Net>) -> Vec<Vec<String>> {
    let base = || {
        vec![
            "add".to_string(),
            "rule".to_string(),
            "inet".to_string(),
            ZECURITY_TABLE.to_string(),
            ZECURITY_CHAIN.to_string(),
        ]
    };
    let mut plan = Vec::new();

    // FIRST rule, always: never capture the CONNECTOR's own egress.
    //
    // The rules below match purely on destination and run for EVERY process on
    // this host. If a connector is co-located with this client, its connection to
    // a resource matches too and gets routed into our TUN — a loop (connector →
    // resource → back into our tunnel) in which the resource never receives
    // anything. The connector stamps its egress sockets with
    // CONNECTOR_EGRESS_MARK; we return before marking so those packets follow
    // normal kernel routing. Anything inserted above this reintroduces the loop.
    let mut egress = base();
    egress.extend([
        "meta".into(),
        "mark".into(),
        format!("{:#x}", crate::appmeta::CONNECTOR_EGRESS_MARK),
        "return".into(),
    ]);
    plan.push(egress);

    // ONE rule for the entire synthetic range, port-agnostic: the whole CIDR is
    // ours and no legitimate traffic to it exists outside the tunnel.
    //
    // Scoped to tcp deliberately. The client has no UDP data path, so a UDP packet
    // steered into the TUN would find no handler and be dropped — the app hangs.
    // Left unmarked it takes the normal route and fails fast instead. "Port-
    // agnostic" is what buys the constant-size ruleset; protocol-agnostic buys
    // nothing and costs a hang.
    if let Some(cidr) = synthetic_cidr {
        let mut rule = base();
        rule.extend([
            "ip".into(),
            "daddr".into(),
            cidr.to_string(),
            // `meta l4proto tcp`, NOT a bare `tcp`. A bare protocol keyword is only
            // valid when it introduces a field match (`tcp dport 443`); on its own
            // nft rejects it as a syntax error. The unit test below originally
            // asserted the string contained " tcp " — which it did — while nft
            // refused the rule outright. Caught only by the live test.
            "meta".into(),
            "l4proto".into(),
            "tcp".into(),
            "meta".into(),
            "mark".into(),
            "set".into(),
            ZECURITY_MARK.to_string(),
        ]);
        plan.push(rule);
    }

    // Pinned-IP resources: unchanged, one rule per (ip, port).
    for flow in flows {
        let mut rule = base();
        rule.extend([
            "ip".into(),
            "daddr".into(),
            flow.ip.to_string(),
            "tcp".into(),
            "dport".into(),
            flow.port.to_string(),
            "meta".into(),
            "mark".into(),
            "set".into(),
            ZECURITY_MARK.to_string(),
        ]);
        plan.push(rule);
    }

    plan
}

/// Networks this host can already reach, for synthetic-CIDR collision checks.
///
/// CGNAT space is genuinely in use — carrier networks and other VPN clients both
/// live in 100.64.0.0/10 — so picking a block blind would eventually route a real
/// destination into our TUN and black-hole it. Best-effort: if `ip` fails we
/// return what we have, and `select_cidr` simply has less to avoid.
pub fn observed_local_networks() -> Vec<Net> {
    let mut out = Vec::new();
    for args in [
        ["-4", "route", "show", "table", "all"].as_slice(),
        ["-4", "addr", "show"].as_slice(),
    ] {
        if let Ok(o) = Command::new("ip").args(args).output() {
            out.extend(parse_observed_networks(&String::from_utf8_lossy(&o.stdout)));
        }
    }
    out
}

/// Pull every `a.b.c.d/len` or bare `a.b.c.d` token out of `ip` output.
///
/// Deliberately token-based rather than format-aware: `ip route` and `ip addr`
/// have different layouts and both change between iproute2 versions, and the
/// cost of a missed network is only a slightly worse CIDR choice. A bare address
/// is treated as a /32.
fn parse_observed_networks(output: &str) -> Vec<Net> {
    let mut nets = Vec::new();
    for token in output.split_whitespace() {
        let (addr, prefix) = match token.split_once('/') {
            Some((a, p)) => match p.parse::<u8>() {
                Ok(p) if p <= 32 => (a, p),
                _ => continue,
            },
            None => (token, 32),
        };
        if let Ok(ip) = addr.parse::<Ipv4Addr>() {
            nets.push(Net::new(ip, prefix));
        }
    }
    nets
}

fn run_command(program: &str, args: &[&str]) -> Result<()> {
    let status = Command::new(program)
        .args(args)
        .status()
        .with_context(|| format!("run {program} {}", args.join(" ")))?;
    if !status.success() {
        anyhow::bail!("{program} {} failed with {status}", args.join(" "));
    }
    Ok(())
}
#[cfg(test)]
mod tests {
    use super::*;

    fn flow(ip: &str, port: u16) -> AllowedFlow {
        AllowedFlow {
            ip: ip.parse().unwrap(),
            port,
        }
    }

    fn cidr() -> Net {
        Net::new("100.64.0.0".parse().unwrap(), 22)
    }

    fn joined(plan: &[Vec<String>]) -> Vec<String> {
        plan.iter().map(|r| r.join(" ")).collect()
    }

    /// The Gate 1 regression, asserted rather than remembered: the connector
    /// egress-return rule must be FIRST. Anything above it recreates the routing
    /// loop that stalled every tunnel.
    #[test]
    fn connector_egress_return_is_always_the_first_rule() {
        let mut flows = BTreeSet::new();
        flows.insert(flow("10.0.0.1", 443));

        for cidr_opt in [None, Some(cidr())] {
            let plan = nft_rule_plan(&flows, cidr_opt);
            let first = plan.first().expect("plan must not be empty").join(" ");
            assert!(
                first.contains("meta mark") && first.ends_with("return"),
                "first rule must be the egress-return, got: {first}"
            );
        }
    }

    /// The scaling property this phase exists for: one rule for the whole range,
    /// no matter how many name-addressed resources there are.
    #[test]
    fn the_synthetic_cidr_costs_exactly_one_rule() {
        let plan = nft_rule_plan(&BTreeSet::new(), Some(cidr()));
        assert_eq!(plan.len(), 2, "egress-return + one CIDR rule");
        assert!(joined(&plan)[1].contains("ip daddr 100.64.0.0/22"));
    }

    /// Port-agnostic (that is the scaling win) but NOT protocol-agnostic: a UDP
    /// packet steered into the TUN has no handler and would hang the app.
    #[test]
    fn the_cidr_rule_is_port_agnostic_but_tcp_only() {
        let plan = nft_rule_plan(&BTreeSet::new(), Some(cidr()));
        let rule = &joined(&plan)[1];
        // `meta l4proto tcp` is the only form nft accepts for a protocol-only
        // match. Asserting the exact token sequence rather than "contains tcp",
        // because the loose version passed while nft rejected the rule as a
        // syntax error — the live test is what found it.
        assert!(
            rule.contains("meta l4proto tcp"),
            "protocol match must be `meta l4proto tcp`, not a bare keyword: {rule}"
        );
        assert!(!rule.contains("dport"), "must not pin a port: {rule}");
    }

    /// Pinned-IP resources must be untouched by this phase — same rule shape as
    /// before, one per (ip, port).
    #[test]
    fn pinned_flows_keep_their_per_ip_port_rules() {
        let mut flows = BTreeSet::new();
        flows.insert(flow("10.0.0.1", 443));
        flows.insert(flow("10.0.0.2", 5432));

        let plan = nft_rule_plan(&flows, None);
        assert_eq!(plan.len(), 3, "egress-return + one rule per flow");
        let rules = joined(&plan);
        assert!(rules[1].contains("ip daddr 10.0.0.1 tcp dport 443 meta mark set"));
        assert!(rules[2].contains("ip daddr 10.0.0.2 tcp dport 5432 meta mark set"));
    }

    /// Both kinds coexist: the CIDR rule does not displace pinned flows.
    #[test]
    fn synthetic_and_pinned_rules_coexist() {
        let mut flows = BTreeSet::new();
        flows.insert(flow("10.0.0.1", 443));
        let plan = nft_rule_plan(&flows, Some(cidr()));
        assert_eq!(plan.len(), 3);
        let rules = joined(&plan);
        assert!(rules[1].contains("100.64.0.0/22"));
        assert!(rules[2].contains("10.0.0.1"));
    }
    #[test]
    fn parses_networks_out_of_ip_route_output() {
        let out = "\
default via 192.168.1.1 dev wlan0 proto dhcp metric 600
100.64.0.0/10 dev tun0 scope link
192.168.1.0/24 dev wlan0 proto kernel scope link src 192.168.1.57
";
        let nets = parse_observed_networks(out);
        assert!(nets.contains(&Net::new("100.64.0.0".parse().unwrap(), 10)));
        assert!(nets.contains(&Net::new("192.168.1.0".parse().unwrap(), 24)));
        // A bare address is a /32, so a host route is still a collision.
        assert!(nets.contains(&Net::new("192.168.1.1".parse().unwrap(), 32)));
    }

    /// The scenario that makes collision-checking necessary rather than
    /// theoretical: another VPN already holds the bottom of CGNAT space.
    #[test]
    fn a_competing_vpn_in_cgnat_pushes_our_block_elsewhere() {
        let nets = parse_observed_networks("100.64.0.0/12 dev tun0 scope link\n");
        let picked = crate::registry::select_cidr(&nets).expect("a free block must remain");
        assert!(
            !nets.iter().any(|n| n.overlaps(&picked)),
            "picked {picked} which collides with the other VPN"
        );
    }

    #[test]
    fn garbage_tokens_are_ignored() {
        assert!(parse_observed_networks("proto kernel scope link metric 600").is_empty());
        assert!(parse_observed_networks("10.0.0.1/99").is_empty());
    }
}
