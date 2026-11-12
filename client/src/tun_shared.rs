//! The platform-neutral half of the TUN data-plane wiring, shared by the
//! Linux module (`tun.rs`: nftables, fwmark, route table 105) and the Windows
//! module (`tun_windows.rs`: Wintun, per-IP routes). Moved verbatim from
//! `tun.rs`; Linux behaviour is unchanged.

use std::collections::BTreeSet;
use std::net::Ipv4Addr;
use std::process::{Command, Stdio};

use anyhow::{Context, Result};

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct AllowedFlow {
    pub ip: Ipv4Addr,
    pub port: u16,
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
}

/// Runs external commands for the Phase 5-C operations. A seam so the exact
/// command sequences are unit-testable without root.
pub(crate) trait CmdRunner {
    fn run(&self, program: &str, args: &[&str], stdin: Option<&str>) -> Result<()>;
    // Used only by the Linux fwmark-rule check.
    #[cfg_attr(windows, allow(dead_code))]
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

pub(crate) fn run_command(program: &str, args: &[&str]) -> Result<()> {
    let status = Command::new(program)
        .args(args)
        .status()
        .with_context(|| format!("run {program} {}", args.join(" ")))?;
    if !status.success() {
        anyhow::bail!("{program} {} failed with {status}", args.join(" "));
    }
    Ok(())
}
