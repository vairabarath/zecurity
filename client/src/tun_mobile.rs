//! Mobile TUN wiring (Android and iOS): the VPN interface comes from the
//! app's VPN service (see `crate::mobile`), configured with one /32 route
//! per allowed resource IP. Compiled as `crate::tun` on both, with the same
//! surface as the Linux and Windows modules.
//!
//! - Android: `VpnService.establish` creates a new interface whose routes
//!   are fixed; the daemon owns its descriptor, and closing it tears the
//!   interface down.
//! - iOS: the extension owns the utun interface. It reconfigures the routes
//!   and the daemon gets a duplicate of the descriptor, so dropping the
//!   daemon's copy never kills the tunnel; tunnel-down removes the routes
//!   through `mobile::release_tun`. utun frames carry a 4-byte address
//!   family header, which `crate::utun::Utun` strips and adds.
//!
//! There is no route delta on either: `daemon::resource_delta_allowed` is
//! false on mobile and every resource change restarts the tunnel. Capture is
//! per IP, not per port (D-W1, as on Windows). The app (Android) or the
//! extension's own traffic (iOS) never enters the tunnel, so the daemon's
//! QUIC and gRPC connections can't loop.

use std::collections::BTreeSet;
use std::net::Ipv4Addr;

use anyhow::{bail, Context, Result};

pub(crate) use crate::tun_shared::{CmdRunner, SystemRunner};
pub use crate::tun_shared::{AllowedFlow, ResourceKernel};

/// The packet device handed to the net_stack.
#[cfg(target_os = "android")]
pub type Device = tun::AsyncDevice;
#[cfg(target_os = "ios")]
pub type Device = crate::utun::Utun<tun::AsyncDevice>;

pub struct TunManager {
    dev: Option<Device>,
    /// Which VPN interface this manager's tunnel uses (0 = none yet).
    generation: u64,
}

impl TunManager {
    /// Nothing to create yet: the interface needs its routes, which
    /// `configure_allowed_flows` supplies.
    pub async fn create() -> Result<Self> {
        Ok(Self {
            dev: None,
            generation: 0,
        })
    }

    /// Asks the VPN service for an interface routing the allowed flows'
    /// destination IPs, and wraps its file descriptor.
    pub fn configure_allowed_flows(&mut self, flows: &[AllowedFlow]) -> Result<()> {
        let ips: Vec<Ipv4Addr> = flows
            .iter()
            .map(|f| f.ip)
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect();
        let (fd, generation) =
            crate::mobile::establish_tun(&ips).context("create the VPN interface")?;
        if fd < 0 {
            bail!(
                "the VPN interface could not be created (VPN permission not granted, or \
                 another VPN app is active)"
            );
        }
        self.generation = generation;
        let mut config = tun::Configuration::default();
        config.raw_fd(fd);
        let dev = tun::create_as_async(&config).context("wrap the VPN interface")?;
        #[cfg(target_os = "ios")]
        let dev = crate::utun::Utun::new(dev);
        self.dev = Some(dev);
        Ok(())
    }

    /// Hand the device to the smoltcp net_stack.
    pub fn take_device(&mut self) -> Option<Device> {
        self.dev.take()
    }

    /// The tunnel is going down: drop the device (Android: this closes the
    /// interface) and remove the routes (iOS).
    pub async fn cleanup(mut self) -> Result<()> {
        drop(self.dev.take());
        crate::mobile::release_tun(std::mem::take(&mut self.generation));
        Ok(())
    }
}

impl Drop for TunManager {
    fn drop(&mut self) {
        crate::mobile::release_tun(std::mem::take(&mut self.generation));
    }
}

/// Never used on mobile (see the module doc): a resource change restarts
/// the tunnel instead.
impl ResourceKernel for TunManager {
    fn apply_nft_desired(&mut self, _flows: &BTreeSet<AllowedFlow>) -> Result<()> {
        Ok(())
    }

    fn add_routes(&mut self, _ips: &BTreeSet<Ipv4Addr>) -> Result<()> {
        bail!("route changes need a new VPN interface on mobile")
    }

    fn del_routes(&mut self, _ips: &BTreeSet<Ipv4Addr>) -> Result<()> {
        bail!("route changes need a new VPN interface on mobile")
    }

    fn ensure_fwmark_rule(&mut self) -> Result<()> {
        Ok(())
    }
}
