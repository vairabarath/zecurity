use std::net::{IpAddr, SocketAddr};
use std::time::Duration;
use tokio::io::AsyncReadExt;
use tokio::net::TcpStream;

pub fn service_from_port(port: u16) -> &'static str {
    match port {
        21    => "FTP",
        22    => "SSH",
        25    => "SMTP",
        53    => "DNS",
        80    => "HTTP",
        110   => "POP3",
        143   => "IMAP",
        443   => "HTTPS",
        445   => "SMB",
        465   => "SMTPS",
        587   => "SMTP",
        993   => "IMAPS",
        995   => "POP3S",
        1433  => "MSSQL",
        1521  => "Oracle",
        2375  => "Docker",
        2376  => "Docker TLS",
        3000  => "Dev Server",
        3306  => "MySQL",
        3389  => "RDP",
        5432  => "PostgreSQL",
        5672  => "RabbitMQ",
        5900  => "VNC",
        6379  => "Redis",
        6443  => "Kubernetes API",
        8080  => "HTTP Proxy",
        8443  => "gRPC/TLS",
        9090  => "Prometheus",
        9200  => "Elasticsearch",
        27017 => "MongoDB",
        _     => "Unknown",
    }
}

pub fn identify_from_banner(banner: &[u8], port: u16) -> &'static str {
    if banner.len() >= 4 && &banner[..4] == b"SSH-" { return "SSH"; }
    if banner.len() >= 4 && (&banner[..4] == b"220 " || &banner[..4] == b"220-") {
        return if port == 21 { "FTP" } else { "SMTP" };
    }
    if banner.len() >= 3 && &banner[..3] == b"+OK" { return "POP3"; }
    if banner.len() >= 4 && &banner[..4] == b"* OK" { return "IMAP"; }
    if banner.len() >= 5 && &banner[..5] == b"HTTP/" { return "HTTP"; }
    if banner.len() >= 4 && &banner[..4] == b"RFB " { return "VNC"; }
    if banner.len() >= 5 && banner[4] == 0x0a { return "MySQL"; }
    if banner.len() >= 4 && (&banner[..4] == b"-ERR" || banner.starts_with(b"-DENIED")) {
        return "Redis";
    }
    service_from_port(port)
}

/// Connect to ip:port, read a banner (300ms window), return (is_open, service_name).
pub async fn detect_service(ip: IpAddr, port: u16, timeout: Duration) -> (bool, String) {
    let addr = SocketAddr::new(ip, port);
    let mut stream = match tokio::time::timeout(timeout, TcpStream::connect(addr)).await {
        Ok(Ok(s)) => s,
        _ => return (false, String::new()),
    };
    let mut buf = [0u8; 256];
    let service = match tokio::time::timeout(Duration::from_millis(300), stream.read(&mut buf)).await {
        Ok(Ok(n)) if n > 0 => identify_from_banner(&buf[..n], port),
        _ => service_from_port(port),
    };
    (true, service.to_string())
}
