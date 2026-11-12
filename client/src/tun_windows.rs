//! Windows TUN wiring: a Wintun adapter named `zecurity0` (through the `tun`
//! crate's Windows backend) plus one `/32` route per allowed resource IP.
//!
//! Compiled as `crate::tun` on Windows, with the same public surface as the
//! Linux `tun.rs` (`TunManager`, `AllowedFlow`, `ResourceKernel`,
//! `CmdRunner`, `SystemRunner`), so `daemon.rs` and `net_stack.rs` are shared.
//!
//! Capture is per IP, not per port (ADR-003 shape, not ADR-009): Windows has
//! no equivalent of the nftables fwmark + route table 105 filter short of a
//! WFP callout driver. Every port of an allowed resource IP therefore enters
//! the tunnel; smoltcp has listeners only for the ACL's ports and resets the
//! rest. Provisional, see docs/client-multi-platform.md (D-W1).
//!
//! `wintun.dll` must sit next to `zecurity-client.exe`; the daemon runs as
//! LocalSystem, which creating an adapter and routes requires.
//!
//! The command renderers are plain functions over the `CmdRunner` seam, so
//! the unit tests at the bottom also run on Linux (`tun_windows_tests` in
//! main.rs compiles this file there under `cfg(test)`).

use std::collections::BTreeSet;
use std::net::Ipv4Addr;

use anyhow::{Context, Result};

pub(crate) use crate::tun_shared::{CmdRunner, SystemRunner};
pub use crate::tun_shared::{AllowedFlow, ResourceKernel};

/// Adapter name; also the interface alias `netsh` addresses routes by.
pub(crate) const ADAPTER_NAME: &str = "zecurity0";
const ADAPTER_ADDR: &str = "100.64.0.1";

/// `netsh` arguments that add a `/32` route for `ip` on the adapter. Active
/// store only: the routes never survive a reboot, matching Linux.
pub(crate) fn add_route_args(ip: Ipv4Addr) -> Vec<String> {
    route_args("add", ip)
}

/// `netsh` arguments that update an existing `/32` route (the "replace" half
/// of Linux's `ip route replace`, used when the add reports it exists).
pub(crate) fn set_route_args(ip: Ipv4Addr) -> Vec<String> {
    route_args("set", ip)
}

/// `netsh` arguments that delete the `/32` route for `ip`.
pub(crate) fn delete_route_args(ip: Ipv4Addr) -> Vec<String> {
    vec![
        "interface".into(),
        "ipv4".into(),
        "delete".into(),
        "route".into(),
        format!("prefix={ip}/32"),
        format!("interface={ADAPTER_NAME}"),
        "store=active".into(),
    ]
}

fn route_args(verb: &str, ip: Ipv4Addr) -> Vec<String> {
    vec![
        "interface".into(),
        "ipv4".into(),
        verb.into(),
        "route".into(),
        format!("prefix={ip}/32"),
        format!("interface={ADAPTER_NAME}"),
        "metric=1".into(),
        "store=active".into(),
    ]
}

/// PowerShell that removes every IPv4 `/32` route a previous daemon left on
/// the adapter (an adapter that outlived a crashed daemon keeps its active
/// routes), except the adapter's own address and the broadcast host route.
pub(crate) fn stale_route_cleanup_script() -> String {
    format!(
        "Get-NetRoute -InterfaceAlias '{ADAPTER_NAME}' -AddressFamily IPv4 -ErrorAction SilentlyContinue \
         | Where-Object {{ $_.DestinationPrefix -like '*/32' -and $_.DestinationPrefix -ne '{ADAPTER_ADDR}/32' \
         -and $_.DestinationPrefix -ne '255.255.255.255/32' }} \
         | Remove-NetRoute -Confirm:$false -ErrorAction SilentlyContinue"
    )
}

fn as_strs(args: &[String]) -> Vec<&str> {
    args.iter().map(String::as_str).collect()
}

/// Adds (or updates) the `/32` route for each IP.
pub(crate) fn add_routes_with(runner: &dyn CmdRunner, ips: &BTreeSet<Ipv4Addr>) -> Result<()> {
    for ip in ips {
        let add = add_route_args(*ip);
        if runner.run("netsh", &as_strs(&add), None).is_err() {
            // netsh fails an add for a route that already exists; update it
            // instead. A failing set is a real error.
            let set = set_route_args(*ip);
            runner
                .run("netsh", &as_strs(&set), None)
                .with_context(|| format!("route {ip}/32 via {ADAPTER_NAME}"))?;
        }
    }
    Ok(())
}

/// Deletes the `/32` route for each IP.
pub(crate) fn del_routes_with(runner: &dyn CmdRunner, ips: &BTreeSet<Ipv4Addr>) -> Result<()> {
    for ip in ips {
        let args = delete_route_args(*ip);
        runner.run("netsh", &as_strs(&args), None)?;
    }
    Ok(())
}

fn cleanup_stale_routes_with(runner: &dyn CmdRunner) {
    let script = stale_route_cleanup_script();
    let _ = runner.run(
        "powershell",
        &["-NoProfile", "-NonInteractive", "-Command", &script],
        None,
    );
}

pub struct TunManager {
    dev: Option<tun::AsyncDevice>,
    policy_ips: Vec<Ipv4Addr>,
}

impl TunManager {
    pub async fn create() -> Result<Self> {
        // Adapter creation blocks (driver install, address setup). The
        // configuration is built on that thread: on Windows it holds a raw
        // pointer and is not Send.
        let dev = tokio::task::spawn_blocking(|| {
            let mut config = tun::Configuration::default();
            config
                .name(ADAPTER_NAME)
                .address(ADAPTER_ADDR)
                .netmask("255.255.255.255")
                .up();
            tun::create_as_async(&config)
        })
        .await
            .context("create Wintun adapter task")?
            .context(
                "create Wintun adapter zecurity0 (wintun.dll must be next to \
                 zecurity-client.exe, and the service must run as LocalSystem)",
            )?;

        // The adapter may have been reopened from a previous run with its
        // routes still attached.
        tokio::task::spawn_blocking(|| cleanup_stale_routes_with(&SystemRunner))
            .await
            .ok();

        Ok(Self {
            dev: Some(dev),
            policy_ips: Vec::new(),
        })
    }

    /// Route the allowed flows' destination IPs into the adapter, replacing
    /// whatever this manager installed before.
    pub fn configure_allowed_flows(&mut self, flows: &[AllowedFlow]) -> Result<()> {
        let old: BTreeSet<Ipv4Addr> = self.policy_ips.drain(..).collect();
        let _ = del_routes_with(&SystemRunner, &old);
        if flows.is_empty() {
            return Ok(());
        }
        let unique_ips: BTreeSet<Ipv4Addr> = flows.iter().map(|flow| flow.ip).collect();
        add_routes_with(&SystemRunner, &unique_ips)?;
        self.policy_ips = unique_ips.into_iter().collect();
        Ok(())
    }

    /// Hand the AsyncDevice to the smoltcp net_stack, keeping the rest of the manager alive.
    pub fn take_device(&mut self) -> Option<tun::AsyncDevice> {
        self.dev.take()
    }

    /// Remove all routes installed in this session and drop the adapter.
    pub async fn cleanup(mut self) -> Result<()> {
        let ips: BTreeSet<Ipv4Addr> = self.policy_ips.drain(..).collect();
        let _ = del_routes_with(&SystemRunner, &ips);
        drop(self.dev.take());
        Ok(())
    }
}

/// Phase 5-C delta operations on the live tunnel. The port-level capture
/// rules and the fwmark rule do not exist on Windows (see the module doc), so
/// those two are no-ops; routes are the whole data-plane state.
impl ResourceKernel for TunManager {
    fn apply_nft_desired(&mut self, _flows: &BTreeSet<AllowedFlow>) -> Result<()> {
        Ok(())
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
        Ok(())
    }
}

impl Drop for TunManager {
    fn drop(&mut self) {
        // Best-effort, as on Linux: cleanup() removes the routes in the normal
        // path; a later create() clears any that linger on a reopened adapter.
        drop(self.dev.take());
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::RefCell;

    #[derive(Default)]
    struct RecordingRunner {
        calls: RefCell<Vec<String>>,
        /// Calls starting with this prefix fail.
        fail_prefix: Option<&'static str>,
    }

    impl CmdRunner for RecordingRunner {
        fn run(&self, program: &str, args: &[&str], _stdin: Option<&str>) -> Result<()> {
            let call = format!("{program} {}", args.join(" "));
            self.calls.borrow_mut().push(call.clone());
            match self.fail_prefix {
                Some(p) if call.starts_with(p) => anyhow::bail!("failed: {call}"),
                _ => Ok(()),
            }
        }
        fn output(&self, program: &str, args: &[&str]) -> Result<String> {
            self.calls
                .borrow_mut()
                .push(format!("{program} {}", args.join(" ")));
            Ok(String::new())
        }
    }

    fn ips(list: &[[u8; 4]]) -> BTreeSet<Ipv4Addr> {
        list.iter().map(|o| Ipv4Addr::from(*o)).collect()
    }

    #[test]
    fn add_routes_is_one_active_host_route_per_ip() {
        let r = RecordingRunner::default();
        add_routes_with(&r, &ips(&[[10, 0, 0, 2], [10, 0, 0, 1]])).unwrap();
        assert_eq!(
            *r.calls.borrow(),
            vec![
                "netsh interface ipv4 add route prefix=10.0.0.1/32 interface=zecurity0 metric=1 store=active",
                "netsh interface ipv4 add route prefix=10.0.0.2/32 interface=zecurity0 metric=1 store=active",
            ]
        );
    }

    #[test]
    fn existing_route_is_updated_instead_of_failing() {
        let r = RecordingRunner {
            fail_prefix: Some("netsh interface ipv4 add"),
            ..Default::default()
        };
        add_routes_with(&r, &ips(&[[10, 0, 0, 1]])).unwrap();
        assert_eq!(
            *r.calls.borrow(),
            vec![
                "netsh interface ipv4 add route prefix=10.0.0.1/32 interface=zecurity0 metric=1 store=active",
                "netsh interface ipv4 set route prefix=10.0.0.1/32 interface=zecurity0 metric=1 store=active",
            ]
        );
    }

    #[test]
    fn route_failure_after_update_is_an_error() {
        let r = RecordingRunner {
            fail_prefix: Some("netsh"),
            ..Default::default()
        };
        assert!(add_routes_with(&r, &ips(&[[10, 0, 0, 1]])).is_err());
    }

    #[test]
    fn del_routes_deletes_each_host_route() {
        let r = RecordingRunner::default();
        del_routes_with(&r, &ips(&[[10, 0, 0, 1]])).unwrap();
        assert_eq!(
            *r.calls.borrow(),
            vec!["netsh interface ipv4 delete route prefix=10.0.0.1/32 interface=zecurity0 store=active"]
        );
    }

    #[test]
    fn stale_cleanup_keeps_the_adapter_and_broadcast_routes() {
        let script = stale_route_cleanup_script();
        assert!(script.contains("-InterfaceAlias 'zecurity0'"));
        assert!(script.contains("-ne '100.64.0.1/32'"));
        assert!(script.contains("-ne '255.255.255.255/32'"));
        assert!(script.contains("Remove-NetRoute -Confirm:$false"));
    }
}
