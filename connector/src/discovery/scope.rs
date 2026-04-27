use ipnet::IpNet;
use std::net::IpAddr;

pub struct ScanScope {
    pub targets: Vec<IpAddr>,
}

pub fn resolve_scope(cidrs: &[String], max_targets: u32) -> Result<ScanScope, String> {
    let mut targets = Vec::new();
    for c in cidrs {
        // Try parsing as a bare IP first, then as CIDR
        let net: IpNet = if c.contains('/') {
            c.parse().map_err(|_| format!("invalid CIDR: {}", c))?
        } else {
            let ip: IpAddr = c.parse().map_err(|_| format!("invalid IP: {}", c))?;
            IpNet::from(ip)
        };
        for ip in net.hosts() {
            if is_invalid_target(&ip) {
                continue;
            }
            targets.push(ip);
            if targets.len() as u32 >= max_targets {
                return Ok(ScanScope { targets });
            }
        }
    }
    Ok(ScanScope { targets })
}

fn is_invalid_target(ip: &IpAddr) -> bool {
    match ip {
        IpAddr::V4(v4) => v4.is_loopback() || v4.is_multicast() || v4.is_unspecified(),
        IpAddr::V6(v6) => v6.is_loopback() || v6.is_multicast() || v6.is_unspecified(),
    }
}
