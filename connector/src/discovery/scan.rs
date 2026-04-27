use std::net::IpAddr;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use tokio::sync::Semaphore;
use tokio::task::JoinSet;
use tracing::{info, warn};

use super::scope;
use super::service_detect::detect_service;
use super::tcp_ping::tcp_connect_ping;

const MAX_TARGETS: u32       = 512;
const MAX_PORTS: usize       = 16;
const MAX_TIMEOUT_SEC: u64   = 60;
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
    pub reachable_from: String,
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
        return ScanReport {
            request_id,
            results: vec![],
            error: Some(format!("too many ports (max {})", MAX_PORTS)),
        };
    }

    let max_targets   = cmd.max_targets.min(MAX_TARGETS);
    let timeout_sec   = cmd.timeout_sec.min(MAX_TIMEOUT_SEC).max(1);
    let probe_timeout = Duration::from_secs(timeout_sec);

    let scope = match scope::resolve_scope(&cmd.targets, max_targets) {
        Ok(s)  => s,
        Err(e) => return ScanReport { request_id, results: vec![], error: Some(e) },
    };

    info!("scan {}: {} targets, {} ports", request_id, scope.targets.len(), cmd.ports.len());

    // Phase 1: host-alive detection (500ms ping on first port)
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

    // Phase 2: banner-grab probe on alive hosts only
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
