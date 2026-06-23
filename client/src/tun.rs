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
                "{ type route hook output priority mangle; policy accept; }",
            ],
        )?;

        for flow in &unique_flows {
            let ip = flow.ip.to_string();
            let port = flow.port.to_string();
            run_command(
                "nft",
                &[
                    "add",
                    "rule",
                    "inet",
                    ZECURITY_TABLE,
                    ZECURITY_CHAIN,
                    "ip",
                    "daddr",
                    &ip,
                    "tcp",
                    "dport",
                    &port,
                    "meta",
                    "mark",
                    "set",
                    ZECURITY_MARK,
                ],
            )?;
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
