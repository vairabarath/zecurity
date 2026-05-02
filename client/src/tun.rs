use std::net::{IpAddr, Ipv4Addr, Ipv6Addr};

use anyhow::{Context, Result};
use futures::TryStreamExt;
use netlink_packet_route::{
    route::{RouteAddress, RouteAttribute, RouteHeader, RouteMessage},
    AddressFamily,
};
use rtnetlink::Handle;

pub struct TunManager {
    dev: Option<tun::AsyncDevice>,
    routes: Vec<IpAddr>,
    if_index: u32,
    handle: Handle,
}

impl TunManager {
    pub async fn create() -> Result<Self> {
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
            routes: Vec::new(),
            if_index,
            handle,
        })
    }

    /// Verify no existing kernel route overlaps with any of the given IPs.
    pub async fn check_conflicts(&self, ips: &[IpAddr]) -> Result<()> {
        let existing = list_routes_v4(&self.handle).await?;
        for ip in ips {
            if existing.iter().any(|(route_ip, _)| route_ip == ip) {
                anyhow::bail!("route conflict: {} is already in the kernel route table", ip);
            }
        }
        Ok(())
    }

    /// Add one /32 host route pointing to zecurity0.
    pub async fn add_route(&mut self, ip: IpAddr) -> Result<()> {
        add_host_route(&self.handle, ip, self.if_index).await?;
        self.routes.push(ip);
        Ok(())
    }

    /// Hand the AsyncDevice to the smoltcp net_stack, keeping the rest of the manager alive.
    pub fn take_device(&mut self) -> Option<tun::AsyncDevice> {
        self.dev.take()
    }

    /// Remove all routes installed in this session and drop the TUN device.
    pub async fn cleanup(mut self) -> Result<()> {
        for ip in self.routes.drain(..) {
            let _ = del_host_route(&self.handle, ip, self.if_index).await;
        }
        drop(self.dev.take());
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

async fn add_host_route(handle: &Handle, ip: IpAddr, if_index: u32) -> Result<()> {
    match ip {
        IpAddr::V4(v4) => handle
            .route()
            .add()
            .v4()
            .destination_prefix(v4, 32)
            .output_interface(if_index)
            .execute()
            .await
            .with_context(|| format!("rtnetlink add route {}/32", ip)),
        IpAddr::V6(v6) => handle
            .route()
            .add()
            .v6()
            .destination_prefix(v6, 128)
            .output_interface(if_index)
            .execute()
            .await
            .with_context(|| format!("rtnetlink add route {}/128", ip)),
    }
}

async fn del_host_route(handle: &Handle, ip: IpAddr, if_index: u32) -> Result<()> {
    let (af, addr, prefix_len) = match ip {
        IpAddr::V4(v4) => (
            AddressFamily::Inet,
            RouteAddress::Inet(v4),
            32u8,
        ),
        IpAddr::V6(v6) => (
            AddressFamily::Inet6,
            RouteAddress::Inet6(v6),
            128u8,
        ),
    };

    let mut msg = RouteMessage::default();
    msg.header.address_family = af;
    msg.header.destination_prefix_length = prefix_len;
    msg.attributes.push(RouteAttribute::Destination(addr));
    msg.attributes.push(RouteAttribute::Oif(if_index));

    handle
        .route()
        .del(msg)
        .execute()
        .await
        .with_context(|| format!("rtnetlink del route {}", ip))
}

async fn list_routes_v4(handle: &Handle) -> Result<Vec<(IpAddr, u8)>> {
    let mut routes = handle
        .route()
        .get(rtnetlink::IpVersion::V4)
        .execute();
    let mut result = Vec::new();
    while let Some(msg) = routes.try_next().await? {
        let prefix_len = msg.header.destination_prefix_length;
        for attr in &msg.attributes {
            if let RouteAttribute::Destination(addr) = attr {
                let ip = match addr {
                    RouteAddress::Inet(v4) => IpAddr::V4(*v4),
                    RouteAddress::Inet6(v6) => IpAddr::V6(*v6),
                    _ => continue,
                };
                result.push((ip, prefix_len));
            }
        }
    }
    Ok(result)
}

// Suppress unused import warnings for types only needed in pattern matching.
#[allow(unused_imports)]
use std::net::{IpAddr as _IpAddr};
