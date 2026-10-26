// main.rs — ZECURITY Connector entry point
//
// Startup flow:
//   1. Load config from env vars + /etc/zecurity/connector.conf  (config.rs)
//   2. Initialize structured logging with the configured log level (tracing)
//   3. Log startup info
//   4. Check state.json in state_dir:
//      - Not exists → run enrollment flow           (enrollment.rs)
//      - Exists     → load saved certs/keys
//   5a. Build ShieldRegistry and spawn shield-facing gRPC server on :9091
//   5b. Spawn auto-updater if enabled               (updater.rs)
//   6. Run bidirectional Control stream to controller (control_stream.rs)
//      — blocks with inner reconnect loop until process shutdown

// All modules live in lib.rs so tests/ can link against them. Main pulls
// them in via the library crate's namespace.

use std::net::SocketAddr;
use std::path::Path;

use std::sync::Arc;

use anyhow::Context;
use enrollment::EnrollmentState;
use tokio::sync::mpsc;
use tracing::{error, info, warn};
use zecurity_connector::{
    agent_server, appmeta, authorization, config::ConnectorConfig, control_stream, controller_client, crl,
    device_tunnel, enrollment, fail_closed, net_util, policy, quic_listener, relay_attachment, relay_handler,
    relay_selector,session_registry, tls, updater, watchdog, ControlMessage,
};
use zecurity_connector::proto;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    rustls::crypto::ring::default_provider()
        .install_default()
        .expect("failed to install default crypto provider");

    if std::env::args().any(|a| a == "--check-update") {
        tracing_subscriber::fmt()
            .with_env_filter(tracing_subscriber::EnvFilter::new("info"))
            .init();
        info!(
            version = env!("CARGO_PKG_VERSION"),
            "running single update check"
        );
        return updater::run_single_check().await;
    }

    let cfg = ConnectorConfig::load()?;

    let env_filter = tracing_subscriber::EnvFilter::try_new(&cfg.log_level)
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
    tracing_subscriber::fmt().with_env_filter(env_filter).init();

    info!(
        product = appmeta::PRODUCT_NAME,
        version = env!("CARGO_PKG_VERSION"),
        controller_addr = %cfg.controller_addr,
        state_dir = %cfg.state_dir,
        "starting connector"
    );

    let state_path = Path::new(&cfg.state_dir).join("state.json");

    let enrollment_state: EnrollmentState = if state_path.exists() {
        let state = EnrollmentState::load(&cfg.state_dir)?;
        info!(
            connector_id = %state.connector_id,
            trust_domain = %state.trust_domain,
            workspace_id = %state.workspace_id,
            enrolled_at = %state.enrolled_at,
            "connector already enrolled"
        );
        state
    } else {
        info!("no state found — starting enrollment");
        let state = enrollment::enroll(&cfg).await?;
        info!(
            connector_id = %state.connector_id,
            trust_domain = %state.trust_domain,
            "enrollment complete"
        );
        state
    };

    // Single owner of the connector certificate (Sprint 20 G-2a). Every TLS
    // consumer reads the CURRENT certificate from it and switches when a
    // renewal is published (verify → atomic persist → publish).
    let certs = tls::cert_holder::CertHolder::load(
        &cfg.state_dir,
        &appmeta::connector_spiffe_id(&enrollment_state.trust_domain, &enrollment_state.connector_id),
    )
    .context("failed to load connector certificate")?;
    // CA bundle (pinned across renewals) for CRL issuer checks and the
    // initial controller channel.
    let cert_store = certs.current().store.clone();

    // Build controller channel for ShieldRegistry (proxies RenewCert to controller).
    let controller_channel = controller_client::build_channel(&cfg, &cert_store)
        .await
        .context("failed to connect to controller for ShieldRegistry")?;

    // Create ack channel shared between ShieldRegistry (producers) and control_stream (consumer).
    let (ack_tx, ack_rx) = mpsc::channel(128);

    // PolicyCache is constructed here (rather than later) so ShieldRegistry
    // can hold an Arc to it — the Shield-facing handler reads the cache on
    // every ShieldHealthReport to piggyback the peer-Connector list.
    let policy_cache = Arc::new(policy::PolicyCache::new());

 let session_registry = Arc::new(session_registry::SessionRegistry::new());

    // Connector-revocation B1: the process-global authorization state. Every
    // start begins in `Starting` (nothing persisted, D12); every data-plane
    // entry path holds its gate and stays closed until the controller's
    // first accepted acknowledgement.
    let authz = authorization::AckTracker::shared(control_stream::AUTHORIZATION_FRESHNESS);
    let gate = authz.lock().gate();
    authorization::spawn_expiry(authz.clone());

    let shield_registry = agent_server::ShieldRegistry::new(
        controller_channel,
        enrollment_state.trust_domain.clone(),
        enrollment_state.connector_id.clone(),
        ack_tx,
        policy_cache.clone(),
        gate.clone(),
    );
    // Connector-revocation fail-closed (B2): on every loss of authorization
    // cancel all sessions; terminal → close shield streams and clear the ACL;
    // Disabled → close shield streams after the D15 grace.
    fail_closed::spawn(
        &gate,
        fail_closed::FailClosed::new(
            session_registry.clone(),
            shield_registry.clone(),
            policy_cache.clone(),
        ),
    );

    // Rebuild the Shield-proxy controller channel with the renewed identity
    // whenever the certificate holder publishes.
    {
        let refresh_cfg = cfg.clone();
        shield_registry.spawn_controller_channel_refresh(certs.clone(), move |material| {
            let cfg = refresh_cfg.clone();
            async move { controller_client::build_channel(&cfg, &material.store).await }
        });
    }

    // Spawn shield-facing gRPC server on :9091.
    let reg_for_serve = shield_registry.clone();
    let shield_certs = certs.clone();
    let shield_addr: SocketAddr = "0.0.0.0:9091".parse().unwrap();
    tokio::spawn(async move {
        if let Err(e) = reg_for_serve.serve(shield_addr, shield_certs).await {
            error!(error = %e, "Shield gRPC server on :9091 failed");
        }
    });
    info!("Shield gRPC server starting on :9091");

    // Spawn auto-updater if enabled.
    if cfg.auto_update_enabled {
        let upd_cfg = cfg.clone();
        tokio::spawn(async move {
            if let Err(e) = updater::run_update_loop(&upd_cfg).await {
                error!(error = %e, "auto-updater failed");
            }
        });
        info!("auto-updater spawned");
    }

    info!("connector running — entering Control stream loop");

    // `policy_cache` already constructed above and shared with ShieldRegistry.

    // Determine LAN IP for QUIC advertise address.
    let lan_ip = net_util::lan_ip()
        .map(|ip| ip.to_string())
        .unwrap_or_default();
    let quic_advertise = format!("{}:9092", lan_ip);

    let acl = policy_cache.clone();
    let tunnel_hub = shield_registry.tunnel_hub.clone();
    let connector_id = enrollment_state.connector_id.clone();

    // Build CRL URL from controller HTTP address (fallback: derive host from gRPC addr + port 8080).
    let http_base = cfg.controller_http_addr.clone().unwrap_or_else(|| {
        let host = cfg
            .controller_addr
            .split(':')
            .next()
            .unwrap_or("localhost")
            .to_string();
        format!("http://{}:8080", host)
    });
    let http_base = enrollment::http_base_url(&http_base);
    let crl_url = format!(
        "{}/ca.crl?workspace_id={}",
        http_base,
        enrollment_state.workspace_id
    );

    let relay_crl_url = format!("{}/relay.crl", http_base.trim_end_matches('/'));
    let (crl_manager, relay_crl_manager) =
        crl::start_crl_managers(&cert_store.workspace_ca_pem, crl_url, relay_crl_url).await;

    // Control message channel for device_tunnel → control_stream (emits access logs).
    let (ctrl_tx, ctrl_rx) = tokio::sync::mpsc::channel::<ControlMessage>(128);

    // Shared relay-attachment state. Written by relay_client on register
    // success / session end; read by control_stream when building each
    // ConnectorHealthReport.
    let relay_attachment_slot = relay_attachment::new_slot();

    // Spawn TLS/TCP device tunnel listener on :9092 (M4 implements; stub for now).
    {
        let store = certs.clone();
        let acl = acl.clone();
        let registry = session_registry.clone();
        let hub = tunnel_hub.clone();
        let crl = crl_manager.clone();
        let cid = connector_id.clone();
        let tx = ctrl_tx.clone();
        let gate = gate.clone();
        tokio::spawn(async move {
            if let Err(e) =
                device_tunnel::listen("0.0.0.0:9092", store, acl,registry, hub, crl, cid, tx, gate).await
            {
                error!(error = %e, "device tunnel (TLS) on :9092 failed");
            }
        });
    }

    // Sprint 20 Fix 16 (F51): cancelled on SIGTERM / SIGINT (graceful shutdown).
    let shutdown = tokio_util::sync::CancellationToken::new();

    // Spawn QUIC/UDP device tunnel listener on :9092.
    let quic_task = {
        let store = certs.clone();
        let acl = acl.clone();
         let registry = session_registry.clone();
        let hub = tunnel_hub.clone();
        let crl = crl_manager.clone();
        let cid = connector_id.clone();
        let tx = ctrl_tx.clone();
        let gate = gate.clone();
        let shutdown = shutdown.clone();
        tokio::spawn(async move {
            if let Err(e) = quic_listener::listen(
                "0.0.0.0:9092",
                &quic_advertise,
                store,
                acl,
                registry,
                hub,
                crl,
                cid,
                tx,
                gate,
                shutdown,
            )
            .await
            {
                error!(error = %e, "device tunnel (QUIC) on :9092 failed");
            }
        })
    };

    info!("device tunnel listeners spawned on :9092 (TLS+QUIC)");

    // Sprint 11 ADR-016: relay selector + watch channel for LabelledRelayList.
    // The controller pushes labelled relays into the channel via the
    // control stream; the selector subscribes, probes them, and runs the
    // make-before-break attachment lifecycle.
    let (relay_list_tx, relay_list_rx) = tokio::sync::watch::channel(None);

    let relay_handler = Arc::new(
        relay_handler::RelayHandler::new(
            certs.clone(),
            acl.clone(),
            session_registry.clone(),
            tunnel_hub.clone(),
            crl_manager.clone(),
            connector_id.clone(),
            ctrl_tx.clone(),
            cfg.relay_inner_handshake_timeout_secs,
            cfg.relay_max_tunnel_streams as usize,
            gate.clone(),
        )
        .context("build Connector Relay stream handler")?,
    );
    let connector_spiffe_id =
        appmeta::connector_spiffe_id(&enrollment_state.trust_domain, &connector_id);
    let selector_cfg = relay_selector::RelaySelectorConfig {
        state_dir: std::path::PathBuf::from(&cfg.state_dir),
        connector_id: connector_id.clone(),
        connector_spiffe_id,
        certs: certs.clone(),
        relay_crl_manager,
        max_incoming_bidi_streams: cfg.relay_max_tunnel_streams,
        idle_timeout: std::time::Duration::from_secs(cfg.relay_idle_timeout_secs),
        reprobe_interval: std::time::Duration::from_secs(cfg.relay_reprobe_interval_secs),
        max_concurrent_probes: cfg.relay_max_concurrent_probes,
        probe_timeout: std::time::Duration::from_millis(3000),
        reconnect_base: std::time::Duration::from_secs(cfg.relay_reconnect_base_secs),
        reconnect_max: std::time::Duration::from_secs(cfg.relay_reconnect_max_secs),
        reconnect_backoff_factor: cfg.relay_reconnect_backoff_factor,
        drain_timeout: std::time::Duration::from_secs(cfg.relay_drain_timeout_secs),
    };
    let selector_attachment_slot = relay_attachment_slot.clone();
    let selector_ctrl_tx = ctrl_tx.clone();
    tokio::spawn(async move {
        relay_selector::run(
            selector_cfg,
            relay_handler,
            selector_attachment_slot,
            relay_list_rx,
            selector_ctrl_tx,
        )
        .await
    });
    info!("Relay selector spawned (ADR-016)");

    watchdog::notify_ready();
    watchdog::spawn_watchdog();

    let shutdown_shields = shield_registry.clone();
    let shutdown_certs = certs.clone();

    // Run bidirectional Control stream to controller (blocks with reconnect loop)
    // until a shutdown signal.
    tokio::select! {
        result = control_stream::run_control_stream(
            &cfg,
            &enrollment_state,
            shield_registry,
            ack_rx,
            ctrl_rx,
            policy_cache,
            session_registry,
            relay_attachment_slot,
            relay_list_tx,
            certs,
            authz,
        ) => result,
        signal = shutdown_signal() => {
            graceful_shutdown(signal, &cfg, &shutdown_certs, &shutdown_shields, &shutdown, quic_task).await;
            Ok(())
        }
    }
}

/// The first of SIGTERM (`systemctl stop`) and SIGINT (Ctrl-C).
async fn shutdown_signal() -> &'static str {
    let mut term = match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
        Ok(term) => term,
        Err(e) => {
            warn!(error = %e, "cannot listen for SIGTERM; only SIGINT stops the connector gracefully");
            let _ = tokio::signal::ctrl_c().await;
            return "SIGINT";
        }
    };
    tokio::select! {
        _ = term.recv() => "SIGTERM",
        _ = tokio::signal::ctrl_c() => "SIGINT",
    }
}

/// Sprint 20 Fix 16 (F51): stop so that everyone notices at once, instead of
/// the process just vanishing (clients waited for their 30 s QUIC idle timeout,
/// the controller for its disconnect threshold):
/// 1. close every client QUIC connection (CONNECTION_CLOSE);
/// 2. close every shield Control stream as not serving, so shields fail over;
/// 3. say Goodbye to the controller, which marks this connector disconnected
///    and pushes the change to clients;
/// all bounded, then return so the process exits.
async fn graceful_shutdown(
    signal: &str,
    cfg: &ConnectorConfig,
    certs: &tls::cert_holder::CertHolder,
    shields: &agent_server::ShieldRegistry,
    shutdown: &tokio_util::sync::CancellationToken,
    quic_task: tokio::task::JoinHandle<()>,
) {
    const GOODBYE_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(2);
    info!(signal, "shutting down: closing client connections and shield streams, saying goodbye");
    shutdown.cancel();
    shields.close_all_streams("connector shutting down");

    let store = certs.current().store.clone();
    let goodbye = async {
        let channel = controller_client::build_channel(cfg, &store).await?;
        proto::connector_service_client::ConnectorServiceClient::new(channel)
            .goodbye(proto::GoodbyeRequest { connector_id: String::new() })
            .await
            .map_err(anyhow::Error::from)
    };
    match tokio::time::timeout(GOODBYE_TIMEOUT, goodbye).await {
        Ok(Ok(_)) => info!("goodbye sent: the controller marked this connector disconnected"),
        Ok(Err(e)) => warn!(error = %e, "goodbye to the controller failed"),
        Err(_) => warn!("goodbye to the controller timed out"),
    }
    // The QUIC listener returns once its close frames went out (bounded).
    let _ = tokio::time::timeout(std::time::Duration::from_secs(3), quic_task).await;
    info!("shutdown complete");
}
