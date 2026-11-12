//! Android TUN wiring: the VPN interface comes from the app's `VpnService`
//! (see `crate::android`), created with one /32 route per allowed resource
//! IP. Compiled as `crate::tun` on Android, with the same surface as the
//! Linux and Windows modules.
//!
//! An Android VPN interface's routes are fixed when it is created, so there
//! is no route delta: `daemon::resource_delta_allowed` is false on Android
//! and every resource change restarts the tunnel, which creates a new
//! interface with the new routes. Capture is per IP, not per port (D-W1,
//! as on Windows). The app excludes itself from its VPN, so the daemon's own
//! QUIC and gRPC traffic never loops into the tunnel.

use std::collections::BTreeSet;
use std::net::Ipv4Addr;

use anyhow::{bail, Context, Result};

pub(crate) use crate::tun_shared::{CmdRunner, SystemRunner};
pub use crate::tun_shared::{AllowedFlow, ResourceKernel};

pub struct TunManager {
    dev: Option<tun::AsyncDevice>,
}

impl TunManager {
    /// Nothing to create yet: the interface needs its routes up front, which
    /// `configure_allowed_flows` supplies.
    pub async fn create() -> Result<Self> {
        Ok(Self { dev: None })
    }

    /// Asks the VpnService for an interface routing the allowed flows'
    /// destination IPs, and wraps its file descriptor.
    pub fn configure_allowed_flows(&mut self, flows: &[AllowedFlow]) -> Result<()> {
        let ips: Vec<Ipv4Addr> = flows
            .iter()
            .map(|f| f.ip)
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect();
        let fd = crate::android::establish_tun(&ips).context("create the VPN interface")?;
        if fd < 0 {
            bail!(
                "the VPN interface could not be created (VPN permission not granted, or \
                 another VPN app is active)"
            );
        }
        let mut config = tun::Configuration::default();
        config.raw_fd(fd);
        let dev = tun::create_as_async(&config).context("wrap the VPN interface")?;
        self.dev = Some(dev);
        Ok(())
    }

    /// Hand the AsyncDevice to the smoltcp net_stack.
    pub fn take_device(&mut self) -> Option<tun::AsyncDevice> {
        self.dev.take()
    }

    /// Closing the file descriptor (owned by the net_stack once taken) tears
    /// the VPN interface down.
    pub async fn cleanup(mut self) -> Result<()> {
        drop(self.dev.take());
        Ok(())
    }
}

/// Never used on Android (see the module doc): a resource change restarts
/// the tunnel instead.
impl ResourceKernel for TunManager {
    fn apply_nft_desired(&mut self, _flows: &BTreeSet<AllowedFlow>) -> Result<()> {
        Ok(())
    }

    fn add_routes(&mut self, _ips: &BTreeSet<Ipv4Addr>) -> Result<()> {
        bail!("route changes need a new VPN interface on Android")
    }

    fn del_routes(&mut self, _ips: &BTreeSet<Ipv4Addr>) -> Result<()> {
        bail!("route changes need a new VPN interface on Android")
    }

    fn ensure_fwmark_rule(&mut self) -> Result<()> {
        Ok(())
    }
}
