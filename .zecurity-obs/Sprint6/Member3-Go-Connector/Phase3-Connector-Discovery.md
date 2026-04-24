---
type: phase
status: pending
sprint: 6
member: M3
phase: Phase3-Connector-Discovery
depends_on:
  - M2-D1-A
  - M2-D1-B
  - buf generate
tags:
  - rust
  - connector
  - discovery
  - scan
---

# M3 Phase 3 — Connector Discovery Modules + Control Stream Wiring

---

## What You're Building

Three things in the Rust connector:

1. **`connector/src/discovery/`** — new module directory with a two-phase network scanner
2. **`connector/src/agent_server.rs`** — relay Shield discovery reports upstream
3. **`connector/src/control_plane.rs`** — handle incoming `ScanCommand` → execute → send `ScanReport`

---

## How the Scan Works (Two-Phase)

**Phase 1 — Host alive detection:** Quick 500ms TCP connect-ping on the first requested port across all targets. `ConnectionRefused` counts as alive — the host is up, just that port is closed. Only hosts that respond proceed to phase 2.

**Phase 2 — Banner-grab probe:** For each alive host, connect to every requested port with the full timeout, read up to 256 bytes of banner, identify the service from the banner bytes (SSH-, 220, +OK, HTTP/, RFB, MySQL byte, Redis -ERR). Falls back to static port→name lookup if banner is unrecognised.

This two-phase approach avoids probing all ports on dead/filtered hosts, which makes large CIDR scans orders of magnitude faster.

---

## Files to Create / Modify

### 1. `connector/src/discovery/mod.rs` (NEW)

```rust
pub mod scan;
pub mod scope;
pub mod service_detect;
pub mod tcp_ping;
```

---

### 2. `connector/src/discovery/tcp_ping.rs` (NEW)

```rust
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
        Err(_)     => false,  // timed out — host unreachable
    }
}
```

---

### 3. `connector/src/discovery/service_detect.rs` (NEW)

Static port lookup + TCP banner grabbing for accurate service identification.

```rust
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

/// Identify service from the first bytes of a TCP banner.
pub fn identify_from_banner(banner: &[u8], port: u16) -> &'static str {
    if banner.len() >= 4 && &banner[..4] == b"SSH-" { return "SSH"; }
    if banner.len() >= 4 && (&banner[..4] == b"220 " || &banner[..4] == b"220-") {
        return if port == 21 { "FTP" } else { "SMTP" };
    }
    if banner.len() >= 3 && &banner[..3] == b"+OK" { return "POP3"; }
    if banner.len() >= 4 && &banner[..4] == b"* OK" { return "IMAP"; }
    if banner.len() >= 5 && &banner[..5] == b"HTTP/" { return "HTTP"; }
    if banner.len() >= 4 && &banner[..4] == b"RFB " { return "VNC"; }
    // MySQL: protocol version byte 0x0a at offset 4
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
```

---

### 4. `connector/src/discovery/scope.rs` (NEW)

CIDR expander using `ipnet` crate. Filters loopback, multicast, unspecified.

```rust
use ipnet::IpNet;
use std::net::IpAddr;

pub struct ScanScope {
    pub targets: Vec<IpAddr>,
}

pub fn resolve_scope(cidrs: &[String], max_targets: u32) -> Result<ScanScope, String> {
    let mut targets = Vec::new();
    for c in cidrs {
        let net: IpNet = c.parse().map_err(|_| format!("invalid CIDR: {}", c))?;
        for ip in net.hosts() {
            if is_invalid_target(&ip) { continue; }
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
```

> Add `ipnet = "2"` to `connector/Cargo.toml`.

---

### 5. `connector/src/discovery/scan.rs` (NEW)

Two-phase executor. Uses `JoinSet` for structured concurrency.

```rust
use std::net::IpAddr;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio::sync::Semaphore;
use tokio::task::JoinSet;
use tracing::{info, warn};

use super::scope;
use super::service_detect::detect_service;
use super::tcp_ping::tcp_connect_ping;

const MAX_TARGETS: u32     = 512;
const MAX_PORTS: usize     = 16;
const MAX_TIMEOUT_SEC: u64 = 60;
const MAX_CONCURRENCY: usize = 32;

pub struct ScanCommand {
    pub request_id:  String,
    pub targets:     Vec<String>,
    pub ports:       Vec<u16>,
    pub max_targets: u32,
    pub timeout_sec: u64,
}

pub struct ScanResult {
    pub ip:             String,
    pub port:           u16,
    pub protocol:       String,
    pub service_name:   String,
    pub reachable_from: String,  // connector_id — records which connector ran the scan
    pub first_seen:     u64,
}

pub struct ScanReport {
    pub request_id: String,
    pub results:    Vec<ScanResult>,
    pub error:      Option<String>,
}

pub async fn execute_scan(cmd: ScanCommand, connector_id: &str) -> ScanReport {
    let request_id = cmd.request_id.clone();

    if cmd.targets.is_empty() {
        return ScanReport { request_id, results: vec![], error: Some("no targets specified".into()) };
    }
    if cmd.ports.is_empty() {
        return ScanReport { request_id, results: vec![], error: Some("no ports specified".into()) };
    }
    if cmd.ports.len() > MAX_PORTS {
        return ScanReport { request_id, results: vec![], error: Some(format!("too many ports (max {})", MAX_PORTS)) };
    }

    let max_targets   = cmd.max_targets.min(MAX_TARGETS);
    let timeout_sec   = cmd.timeout_sec.min(MAX_TIMEOUT_SEC).max(1);
    let probe_timeout = Duration::from_secs(timeout_sec);

    let scope = match scope::resolve_scope(&cmd.targets, max_targets) {
        Ok(s) => s,
        Err(e) => return ScanReport { request_id, results: vec![], error: Some(e) },
    };

    info!("scan {}: {} targets, {} ports", request_id, scope.targets.len(), cmd.ports.len());

    // ── Phase 1: host-alive detection (500ms ping on first port) ─────────────
    let ping_port    = cmd.ports[0];
    let ping_timeout = Duration::from_millis(500);
    let sem          = Arc::new(Semaphore::new(MAX_CONCURRENCY));
    let mut ping_set: JoinSet<(IpAddr, bool)> = JoinSet::new();

    for ip in &scope.targets {
        let ip  = *ip;
        let sem = sem.clone();
        ping_set.spawn(async move {
            let _permit = sem.acquire().await.unwrap();
            (ip, tcp_connect_ping(ip, ping_port, ping_timeout).await)
        });
    }

    let mut alive: Vec<IpAddr> = Vec::new();
    while let Some(Ok((ip, is_alive))) = ping_set.join_next().await {
        if is_alive {
            info!("alive: {}", ip);
            alive.push(ip);
        }
    }

    if alive.is_empty() {
        warn!("scan {}: no alive hosts found", request_id);
        return ScanReport { request_id, results: vec![], error: None };
    }

    // ── Phase 2: banner-grab probe on alive hosts only ───────────────────────
    let sem = Arc::new(Semaphore::new(MAX_CONCURRENCY));
    let mut probe_set: JoinSet<(IpAddr, u16, bool, String)> = JoinSet::new();

    for ip in &alive {
        for &port in &cmd.ports {
            let ip  = *ip;
            let sem = sem.clone();
            let t   = probe_timeout;
            probe_set.spawn(async move {
                let _permit = sem.acquire().await.unwrap();
                let (open, svc) = detect_service(ip, port, t).await;
                (ip, port, open, svc)
            });
        }
    }

    let now = SystemTime::now().duration_since(UNIX_EPOCH).unwrap_or_default().as_secs();
    let mut results = Vec::new();

    while let Some(Ok((ip, port, true, service_name))) = probe_set.join_next().await {
        info!("open: {}:{} ({})", ip, port, service_name);
        results.push(ScanResult {
            ip:             ip.to_string(),
            port,
            protocol:       "tcp".into(),
            service_name,
            reachable_from: connector_id.to_string(),
            first_seen:     now,
        });
    }

    info!("scan {}: {} services found", request_id, results.len());
    ScanReport { request_id, results, error: None }
}
```

---

### 6. `connector/src/agent_server.rs` (MODIFY)

When Shield sends `ShieldControlMessage::DiscoveryReport` on the Control stream:
- Buffer in `ShieldState`: add `pending_discovery: Option<proto::shieldv1::DiscoveryReport>`
- On a 5s flush ticker (or when buffer is non-empty): wrap in `ShieldDiscoveryBatch` → send as `ConnectorControlMessage::ShieldDiscovery` upstream
- Clear buffer after sending

---

### 7. `connector/src/control_plane.rs` (MODIFY)

Handle `ConnectorControlMessage::ScanCommand` from Controller:

```rust
ConnectorControlMessage::ScanCommand(cmd) => {
    let connector_id = self.connector_id.clone();
    let upstream_tx  = self.upstream_tx.clone();
    tokio::spawn(async move {
        let scan_cmd = discovery::scan::ScanCommand {
            request_id:  cmd.request_id.clone(),
            targets:     cmd.targets,
            ports:       cmd.ports.into_iter().map(|p| p as u16).collect(),
            max_targets: cmd.max_targets,
            timeout_sec: cmd.timeout_sec as u64,
        };
        let report = discovery::scan::execute_scan(scan_cmd, &connector_id).await;
        let _ = upstream_tx.send(proto_from_scan_report(report)).await;
    });
}
```

Add `mod discovery;` to `connector/src/main.rs`.

---

## Build Check

```bash
cd connector && cargo build
```

Warnings OK, errors not.
