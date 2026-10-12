use std::collections::BTreeSet;
use std::net::Ipv4Addr;
use std::process::{Command, Stdio};

use anyhow::{Context, Result};
use futures::TryStreamExt;
use rtnetlink::Handle;

const ZECURITY_TABLE: &str = "zecurity_client";
const ZECURITY_CHAIN: &str = "output";
const ZECURITY_MARK: &str = "0x5a";
const ZECURITY_ROUTE_TABLE: &str = "105";
const ZECURITY_RULE_PRIORITY: &str = "49";
const ZECURITY_CHAIN_SPEC: &str = "{ type route hook output priority mangle; policy accept; }";
const FWMARK_RULE_ADD_ARGS: [&str; 8] = [
    "rule",
    "add",
    "fwmark",
    ZECURITY_MARK,
    "lookup",
    ZECURITY_ROUTE_TABLE,
    "priority",
    ZECURITY_RULE_PRIORITY,
];

/// The match + action of one capture rule (`ip daddr X tcp dport P meta mark
/// set 0x5a`). Shared by the startup path (`configure_allowed_flows`) and the
/// Phase 5-C desired-state batch so the two can never produce different rules.
fn mark_rule_match(flow: &AllowedFlow) -> Vec<String> {
    vec![
        "ip".into(),
        "daddr".into(),
        flow.ip.to_string(),
        "tcp".into(),
        "dport".into(),
        flow.port.to_string(),
        "meta".into(),
        "mark".into(),
        "set".into(),
        ZECURITY_MARK.into(),
    ]
}

/// Phase 5-C: the complete desired capture state as ONE nft transaction
/// (`nft -f -`). `add table`/`add chain` are idempotent; `flush chain` + the
/// rule adds commit together, so the old rules stay in force until the swap
/// and a failing command aborts the whole batch (no handle-based deletes).
/// An empty flow set yields an empty chain: nothing is captured.
pub fn render_nft_batch(flows: &BTreeSet<AllowedFlow>) -> String {
    let mut out = String::new();
    out.push_str(&format!("add table inet {ZECURITY_TABLE}\n"));
    out.push_str(&format!(
        "add chain inet {ZECURITY_TABLE} {ZECURITY_CHAIN} {ZECURITY_CHAIN_SPEC}\n"
    ));
    out.push_str(&format!(
        "flush chain inet {ZECURITY_TABLE} {ZECURITY_CHAIN}\n"
    ));
    for flow in flows {
        out.push_str(&format!(
            "add rule inet {ZECURITY_TABLE} {ZECURITY_CHAIN} {}\n",
            mark_rule_match(flow).join(" ")
        ));
    }
    out
}

/// Phase 5-C: incremental, delta-oriented data-plane operations used by the
/// resource hot-apply. None of them touches the TUN link, and none ever
/// removes the tunnel-level fwmark rule (it stays while the VPN is up, even
/// with zero resources).
pub trait ResourceKernel {
    /// Replace the capture rules with exactly `flows` (one nft transaction).
    fn apply_nft_desired(&mut self, flows: &BTreeSet<AllowedFlow>) -> Result<()>;
    /// `ip route replace X/32 dev zecurity0 table 105` per IP.
    fn add_routes(&mut self, ips: &BTreeSet<Ipv4Addr>) -> Result<()>;
    /// `ip route del X/32 table 105` per IP.
    fn del_routes(&mut self, ips: &BTreeSet<Ipv4Addr>) -> Result<()>;
    /// Ensure exactly one `fwmark 0x5a lookup 105` rule exists; never adds a
    /// duplicate and never deletes it.
    fn ensure_fwmark_rule(&mut self) -> Result<()>;
    /// Usable smoltcp interface-address capacity (an apply-environment query,
    /// overridable in tests). Default: smoltcp's compile-time limit.
    fn addr_capacity(&self) -> usize {
        smoltcp::config::IFACE_MAX_ADDR_COUNT
    }
}

/// Runs external commands for the Phase 5-C operations. A seam so the exact
/// command sequences are unit-testable without root.
pub(crate) trait CmdRunner {
    fn run(&self, program: &str, args: &[&str], stdin: Option<&str>) -> Result<()>;
    fn output(&self, program: &str, args: &[&str]) -> Result<String>;
}

pub(crate) struct SystemRunner;

impl CmdRunner for SystemRunner {
    fn run(&self, program: &str, args: &[&str], stdin: Option<&str>) -> Result<()> {
        match stdin {
            None => run_command(program, args),
            Some(input) => {
                use std::io::Write;
                let mut child = Command::new(program)
                    .args(args)
                    .stdin(Stdio::piped())
                    .spawn()
                    .with_context(|| format!("run {program} {}", args.join(" ")))?;
                child
                    .stdin
                    .take()
                    .context("child stdin")?
                    .write_all(input.as_bytes())
                    .with_context(|| format!("write stdin to {program}"))?;
                let status = child
                    .wait()
                    .with_context(|| format!("wait {program} {}", args.join(" ")))?;
                if !status.success() {
                    anyhow::bail!("{program} {} failed with {status}", args.join(" "));
                }
                Ok(())
            }
        }
    }

    fn output(&self, program: &str, args: &[&str]) -> Result<String> {
        let out = Command::new(program)
            .args(args)
            .output()
            .with_context(|| format!("run {program} {}", args.join(" ")))?;
        if !out.status.success() {
            anyhow::bail!("{program} {} failed with {}", args.join(" "), out.status);
        }
        Ok(String::from_utf8_lossy(&out.stdout).into_owned())
    }
}

pub(crate) fn nft_apply_desired_with(
    runner: &dyn CmdRunner,
    flows: &BTreeSet<AllowedFlow>,
) -> Result<()> {
    runner.run("nft", &["-f", "-"], Some(&render_nft_batch(flows)))
}

pub(crate) fn add_routes_with(runner: &dyn CmdRunner, ips: &BTreeSet<Ipv4Addr>) -> Result<()> {
    for ip in ips {
        let prefix = format!("{ip}/32");
        runner.run(
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
            None,
        )?;
    }
    Ok(())
}

pub(crate) fn del_routes_with(runner: &dyn CmdRunner, ips: &BTreeSet<Ipv4Addr>) -> Result<()> {
    for ip in ips {
        let prefix = format!("{ip}/32");
        runner.run(
            "ip",
            &["route", "del", &prefix, "table", ZECURITY_ROUTE_TABLE],
            None,
        )?;
    }
    Ok(())
}

/// True when `ip rule show` output already contains the Zecurity fwmark rule.
fn has_fwmark_rule(rules: &str) -> bool {
    rules.lines().any(|line| {
        line.contains(&format!("fwmark {ZECURITY_MARK}"))
            && line.contains(&format!("lookup {ZECURITY_ROUTE_TABLE}"))
    })
}

pub(crate) fn ensure_fwmark_rule_with(runner: &dyn CmdRunner) -> Result<()> {
    let rules = runner.output("ip", &["rule", "show"])?;
    if has_fwmark_rule(&rules) {
        return Ok(());
    }
    runner.run("ip", &FWMARK_RULE_ADD_ARGS, None)
}

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

    /// Route only explicitly allowed TCP destination flows into zecurity0.
    ///
    /// nft marks matching local outbound packets before route lookup. The fwmark
    /// rule then selects table 105, where only the allowed destination IPs point
    /// at zecurity0. Other ports on the same IP remain unmarked and use the
    /// normal kernel route.
    pub fn configure_allowed_flows(&mut self, flows: &[AllowedFlow]) -> Result<()> {
        cleanup_policy_routes();
        if flows.is_empty() {
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
                ZECURITY_CHAIN_SPEC,
            ],
        )?;

        for flow in &unique_flows {
            let mut args: Vec<String> = ["add", "rule", "inet", ZECURITY_TABLE, ZECURITY_CHAIN]
                .iter()
                .map(|s| s.to_string())
                .collect();
            args.extend(mark_rule_match(flow));
            let args: Vec<&str> = args.iter().map(String::as_str).collect();
            run_command("nft", &args)?;
        }

        run_command("ip", &FWMARK_RULE_ADD_ARGS)?;

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
}

/// Phase 5-C delta operations on the live tunnel. Never calls
/// `cleanup_policy_routes()` (the flush-everything path stays reserved for
/// startup and `cleanup`).
impl ResourceKernel for TunManager {
    fn apply_nft_desired(&mut self, flows: &BTreeSet<AllowedFlow>) -> Result<()> {
        nft_apply_desired_with(&SystemRunner, flows)
    }

    fn add_routes(&mut self, ips: &BTreeSet<Ipv4Addr>) -> Result<()> {
        add_routes_with(&SystemRunner, ips)?;
        for ip in ips {
            if !self.policy_ips.contains(ip) {
                self.policy_ips.push(*ip);
            }
        }
        Ok(())
    }

    fn del_routes(&mut self, ips: &BTreeSet<Ipv4Addr>) -> Result<()> {
        del_routes_with(&SystemRunner, ips)?;
        self.policy_ips.retain(|ip| !ips.contains(ip));
        Ok(())
    }

    fn ensure_fwmark_rule(&mut self) -> Result<()> {
        ensure_fwmark_rule_with(&SystemRunner)
    }
}

impl TunManager {
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
    //! Phase 5-C: the desired-state nft batch, per-IP routes and the
    //! idempotent fwmark rule, asserted through the command-runner seam
    //! (no root, no kernel).
    use super::*;
    use std::cell::RefCell;

    #[derive(Default)]
    struct RecordingRunner {
        calls: RefCell<Vec<String>>,
        rules_output: String,
    }

    impl CmdRunner for RecordingRunner {
        fn run(&self, program: &str, args: &[&str], stdin: Option<&str>) -> Result<()> {
            let mut call = format!("{program} {}", args.join(" "));
            if let Some(input) = stdin {
                call.push_str(&format!(" <<{input}"));
            }
            self.calls.borrow_mut().push(call);
            Ok(())
        }
        fn output(&self, program: &str, args: &[&str]) -> Result<String> {
            self.calls
                .borrow_mut()
                .push(format!("{program} {}", args.join(" ")));
            Ok(self.rules_output.clone())
        }
    }

    fn flow(ip: [u8; 4], port: u16) -> AllowedFlow {
        AllowedFlow {
            ip: Ipv4Addr::from(ip),
            port,
        }
    }

    #[test]
    fn nft_batch_is_desired_state_flush_then_rules() {
        let flows: BTreeSet<_> = [flow([10, 0, 0, 2], 443), flow([10, 0, 0, 1], 80)]
            .into_iter()
            .collect();
        let batch = render_nft_batch(&flows);
        let lines: Vec<&str> = batch.lines().collect();
        assert_eq!(
            lines,
            vec![
                "add table inet zecurity_client",
                "add chain inet zecurity_client output { type route hook output priority mangle; policy accept; }",
                "flush chain inet zecurity_client output",
                "add rule inet zecurity_client output ip daddr 10.0.0.1 tcp dport 80 meta mark set 0x5a",
                "add rule inet zecurity_client output ip daddr 10.0.0.2 tcp dport 443 meta mark set 0x5a",
            ]
        );
        assert!(!batch.contains("delete"), "no handle-based deletes");
    }

    /// Zero resources: the chain is flushed and left empty (nothing captured),
    /// the table/chain stay (no delete), and nothing touches the fwmark rule.
    #[test]
    fn nft_batch_for_zero_resources_is_an_empty_chain() {
        let batch = render_nft_batch(&BTreeSet::new());
        assert_eq!(batch.lines().count(), 3);
        assert!(batch.contains("flush chain inet zecurity_client output"));
        assert!(!batch.contains("add rule"));
        assert!(!batch.contains("delete"));
        assert!(!batch.contains("fwmark"));
    }

    #[test]
    fn nft_apply_is_one_invocation_from_stdin() {
        let r = RecordingRunner::default();
        let flows: BTreeSet<_> = [flow([10, 0, 0, 1], 80)].into_iter().collect();
        nft_apply_desired_with(&r, &flows).unwrap();
        let calls = r.calls.borrow();
        assert_eq!(calls.len(), 1, "one nft transaction");
        assert!(calls[0].starts_with("nft -f - <<add table inet zecurity_client"));
    }

    #[test]
    fn routes_are_per_ip_in_table_105() {
        let r = RecordingRunner::default();
        let ips: BTreeSet<Ipv4Addr> = [Ipv4Addr::new(10, 0, 0, 2), Ipv4Addr::new(10, 0, 0, 1)]
            .into_iter()
            .collect();
        add_routes_with(&r, &ips).unwrap();
        del_routes_with(&r, &ips).unwrap();
        assert_eq!(
            *r.calls.borrow(),
            vec![
                "ip route replace 10.0.0.1/32 dev zecurity0 table 105",
                "ip route replace 10.0.0.2/32 dev zecurity0 table 105",
                "ip route del 10.0.0.1/32 table 105",
                "ip route del 10.0.0.2/32 table 105",
            ]
        );
    }

    #[test]
    fn fwmark_rule_is_added_only_when_absent() {
        let r = RecordingRunner {
            rules_output: "0:\tfrom all lookup local\n32766:\tfrom all lookup main\n".into(),
            ..Default::default()
        };
        ensure_fwmark_rule_with(&r).unwrap();
        assert_eq!(
            *r.calls.borrow(),
            vec![
                "ip rule show",
                "ip rule add fwmark 0x5a lookup 105 priority 49"
            ]
        );
    }

    #[test]
    fn fwmark_rule_present_is_never_duplicated() {
        let r = RecordingRunner {
            rules_output: "0:\tfrom all lookup local\n49:\tfrom all fwmark 0x5a lookup 105\n"
                .into(),
            ..Default::default()
        };
        ensure_fwmark_rule_with(&r).unwrap();
        ensure_fwmark_rule_with(&r).unwrap();
        assert_eq!(
            *r.calls.borrow(),
            vec!["ip rule show", "ip rule show"],
            "no `ip rule add` when the rule exists"
        );
    }
}
