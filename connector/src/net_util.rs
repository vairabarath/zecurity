use std::net::{IpAddr, UdpSocket};

/// Returns the connector's outbound LAN IP by asking the OS which source IP
/// it would use to reach 8.8.8.8. No packet is sent.
pub fn lan_ip() -> anyhow::Result<IpAddr> {
    let socket = UdpSocket::bind("0.0.0.0:0")?;
    socket.connect("8.8.8.8:53")?;
    let addr = socket.local_addr()?;
    Ok(addr.ip())
}
