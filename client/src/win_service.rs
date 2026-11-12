//! Windows Service hosting for `zecurity-client daemon`: the Windows
//! counterpart of the systemd unit (`systemd/zecurity-client.service`).
//!
//! The service runs as LocalSystem, which creating the Wintun adapter and
//! routes requires (Windows has no `CAP_NET_ADMIN` to delegate). It is
//! installed by `scripts/client-install.ps1`.
//!
//! Started outside the Service Control Manager (for example from an elevated
//! console while debugging), the daemon simply runs in the foreground.

use std::ffi::OsString;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::OnceLock;
use std::time::Duration;

use anyhow::{Context, Result};
use windows_service::service::{
    ServiceControl, ServiceControlAccept, ServiceExitCode, ServiceState, ServiceStatus,
    ServiceType,
};
use windows_service::service_control_handler::{
    self, ServiceControlHandlerResult, ServiceStatusHandle,
};
use windows_service::{define_windows_service, service_dispatcher};

/// Service name; `ipc::ensure_daemon_and_send` starts it by this name.
pub const SERVICE_NAME: &str = "zecurity-client";

/// `ERROR_FAILED_SERVICE_CONTROLLER_CONNECT`: not started by the SCM.
const ERROR_FAILED_SERVICE_CONTROLLER_CONNECT: i32 = 1063;

static STATUS_HANDLE: OnceLock<ServiceStatusHandle> = OnceLock::new();
static RUNNING_AS_SERVICE: AtomicBool = AtomicBool::new(false);

define_windows_service!(ffi_service_main, service_main);

/// Entry point for `zecurity-client daemon` on Windows. `ipc_user_sid` is
/// the enrolling user's SID from the (admin-only) service command line.
pub fn run(ipc_user_sid: Option<String>) -> Result<()> {
    if let Some(sid) = ipc_user_sid.as_deref() {
        if !crate::win_security::is_valid_sid(sid) {
            anyhow::bail!("--ipc-user-sid {sid:?} is not a valid SID");
        }
    }
    crate::ipc::set_pipe_user_sid(ipc_user_sid);
    match service_dispatcher::start(SERVICE_NAME, ffi_service_main) {
        Ok(()) => Ok(()),
        Err(windows_service::Error::Winapi(e))
            if e.raw_os_error() == Some(ERROR_FAILED_SERVICE_CONTROLLER_CONNECT) =>
        {
            secure_data_dir()?;
            runtime()?.block_on(crate::daemon::run())
        }
        Err(e) => Err(e).context("start the Windows service dispatcher"),
    }
}

/// True while the daemon is running under the Service Control Manager.
pub fn running_as_service() -> bool {
    RUNNING_AS_SERVICE.load(Ordering::SeqCst)
}

/// Daemon log file used when running as a service, where stdout goes nowhere
/// (the Linux daemon logs to the journal instead).
pub fn log_path() -> PathBuf {
    crate::config::program_data_dir()
        .join("logs")
        .join("daemon.log")
}

/// Reports the service as stopped. A no-op outside the SCM.
pub fn report_stopped(code: i32) {
    if !running_as_service() {
        return;
    }
    if let Some(handle) = STATUS_HANDLE.get() {
        let exit_code = if code == 0 {
            ServiceExitCode::Win32(0)
        } else {
            ServiceExitCode::ServiceSpecific(code as u32)
        };
        let _ = handle.set_service_status(status(ServiceState::Stopped, exit_code));
    }
}

/// Locks down `%ProgramData%\Zecurity` before the daemon reads anything
/// from it (see win_security). A failure stops the daemon: it must not use
/// a directory someone else controls.
fn secure_data_dir() -> Result<()> {
    if crate::ipc::pipe_user_sid().is_none() {
        eprintln!(
            "zecurity-client: no --ipc-user-sid; only SYSTEM and Administrators can use the daemon"
        );
    }
    crate::win_security::secure_data_dir(crate::ipc::pipe_user_sid())
        .context("secure %ProgramData%\\Zecurity")
}

fn runtime() -> Result<tokio::runtime::Runtime> {
    tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .context("build the tokio runtime")
}

fn status(state: ServiceState, exit_code: ServiceExitCode) -> ServiceStatus {
    ServiceStatus {
        service_type: ServiceType::OWN_PROCESS,
        current_state: state,
        controls_accepted: if state == ServiceState::Running {
            ServiceControlAccept::STOP | ServiceControlAccept::SHUTDOWN
        } else {
            ServiceControlAccept::empty()
        },
        exit_code,
        checkpoint: 0,
        wait_hint: Duration::default(),
        process_id: None,
    }
}

fn service_main(_arguments: Vec<OsString>) {
    if let Err(e) = run_service() {
        tracing::error!(error = %e, "zecurity-client service failed");
        report_stopped(1);
    }
}

fn run_service() -> Result<()> {
    let (stop_tx, stop_rx) = std::sync::mpsc::channel::<()>();
    let handler = move |control| match control {
        ServiceControl::Stop | ServiceControl::Shutdown => {
            let _ = stop_tx.send(());
            ServiceControlHandlerResult::NoError
        }
        ServiceControl::Interrogate => ServiceControlHandlerResult::NoError,
        _ => ServiceControlHandlerResult::NotImplemented,
    };
    let handle = service_control_handler::register(SERVICE_NAME, handler)
        .context("register the service control handler")?;
    let _ = STATUS_HANDLE.set(handle);
    RUNNING_AS_SERVICE.store(true, Ordering::SeqCst);
    handle
        .set_service_status(status(ServiceState::Running, ServiceExitCode::Win32(0)))
        .context("report the service as running")?;

    // Before the daemon reads config or state. service_main reports the
    // failure (exit code 1 in `sc query`); nothing is logged to the file yet.
    secure_data_dir()?;

    // daemon::run only returns on a startup error; deliberate exits go
    // through crate::exit_daemon, which reports Stopped first.
    std::thread::spawn(|| {
        let result = runtime().and_then(|rt| rt.block_on(crate::daemon::run()));
        if let Err(e) = result {
            tracing::error!(error = %e, "daemon exited with an error");
            crate::exit_daemon(1);
        }
    });

    // Stop or system shutdown. As on Linux (systemd SIGTERM), the process
    // simply ends; Wintun removes the adapter, and its routes, with it.
    let _ = stop_rx.recv();
    crate::exit_daemon(0)
}
