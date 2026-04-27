use std::net::{IpAddr, SocketAddr};
use std::time::Duration;
use tokio::net::TcpStream;

/// Returns true if the host is reachable.
/// ConnectionRefused also returns true — the host is up, just that port is closed.
pub async fn tcp_connect_ping(ip: IpAddr, port: u16, timeout: Duration) -> bool {
    let addr = SocketAddr::new(ip, port);
    match tokio::time::timeout(timeout, TcpStream::connect(addr)).await {
        Ok(Ok(_))  => true,
        Ok(Err(e)) => matches!(e.kind(), std::io::ErrorKind::ConnectionRefused),
        Err(_)     => false,
    }
}
