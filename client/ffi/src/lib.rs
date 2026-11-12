//! The Zecurity mobile app's native library (`libzecurity_ffi.so`).
//!
//! Two callers, in two processes of the same app:
//!
//! - **The VPN service process** (Kotlin, JNI): `ZecurityNative.startDaemon`
//!   runs the unchanged client daemon in that process, and registers the TUN
//!   provider through which the daemon asks the service for a VPN interface
//!   (`ZecurityNative.establish`). Hosting the daemon there keeps its
//!   deliberate exits (shutdown request, revoked device) away from the UI.
//! - **The UI process** (Flutter, `dart:ffi`): the C functions below play the
//!   Linux CLI's role. `zc_request` sends one daemon IPC request (the same
//!   newline-delimited JSON the CLI uses) and returns the JSON response.
//!
//! Every function returns a JSON string the caller must free with
//! `zc_free`. All of them block, so the UI calls them off its main isolate.
//!
//! Android only for now (iOS would reuse the C functions).
#![cfg(target_os = "android")]

use std::ffi::{c_char, CStr, CString};
use std::path::PathBuf;
use std::sync::{Mutex, OnceLock};

use serde::Deserialize;
use serde_json::{json, Value};
use zecurity_core::{android, cmd, config, ipc};

fn runtime() -> &'static tokio::runtime::Runtime {
    static RT: OnceLock<tokio::runtime::Runtime> = OnceLock::new();
    RT.get_or_init(|| {
        tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
            .expect("build the tokio runtime")
    })
}

fn install_crypto_provider() {
    let _ = rustls::crypto::ring::default_provider().install_default();
}

fn to_c(value: Value) -> *mut c_char {
    CString::new(value.to_string())
        .unwrap_or_else(|_| CString::new("{}").expect("static"))
        .into_raw()
}

/// # Safety
/// `p` must be NULL or a NUL-terminated string valid for the call.
unsafe fn from_c(p: *const c_char) -> String {
    if p.is_null() {
        return String::new();
    }
    CStr::from_ptr(p).to_string_lossy().into_owned()
}

fn error(e: impl std::fmt::Display) -> *mut c_char {
    to_c(json!({ "ok": false, "type": "Error", "error": e.to_string() }))
}

/// Frees a string returned by any `zc_*` function.
///
/// # Safety
/// `p` must come from a `zc_*` function and not be freed twice.
#[no_mangle]
pub unsafe extern "C" fn zc_free(p: *mut c_char) {
    if !p.is_null() {
        drop(CString::from_raw(p));
    }
}

// ── Login state shared with the UI's polling ─────────────────────────────────

#[derive(Default)]
struct LoginState {
    running: bool,
    url: Option<String>,
    result: Option<Result<(), String>>,
}

fn login_state() -> &'static Mutex<LoginState> {
    static STATE: OnceLock<Mutex<LoginState>> = OnceLock::new();
    STATE.get_or_init(Default::default)
}

/// UI process start-up: the app's files directory (config, state, the
/// daemon socket), and the login URL hand-off to the UI.
///
/// # Safety
/// `files_dir` must be a NUL-terminated UTF-8 path.
#[no_mangle]
pub unsafe extern "C" fn zc_init(files_dir: *const c_char) -> *mut c_char {
    install_crypto_provider();
    android::init(PathBuf::from(from_c(files_dir)));
    android::set_url_opener(Box::new(|url| {
        if let Ok(mut s) = login_state().lock() {
            s.url = Some(url.to_string());
        }
    }));
    to_c(json!({ "ok": true }))
}

/// The current config, for the settings form.
#[no_mangle]
pub extern "C" fn zc_config() -> *mut c_char {
    match config::load() {
        Ok(c) => to_c(json!({
            "ok": true,
            "workspace": c.workspace,
            "controller": c.controller_address,
            "http_base": c.http_base_url,
        })),
        Err(_) => to_c(json!({ "ok": true, "workspace": "", "controller": "", "http_base": "" })),
    }
}

#[derive(Deserialize)]
struct SetupArgs {
    workspace: String,
    #[serde(default)]
    controller: String,
    #[serde(default)]
    http_base: String,
}

/// Writes the config (what `zecurity-client setup` does). The daemon reads
/// it at start, so the app restarts the VPN service afterwards.
///
/// # Safety
/// `args` must be a NUL-terminated JSON object
/// `{"workspace", "controller"?, "http_base"?}`.
#[no_mangle]
pub unsafe extern "C" fn zc_setup(args: *const c_char) -> *mut c_char {
    let args: SetupArgs = match serde_json::from_str(&from_c(args)) {
        Ok(a) => a,
        Err(e) => return error(format!("bad setup arguments: {e}")),
    };
    if args.workspace.trim().is_empty() {
        return error("workspace is required");
    }
    let conf = config::ClientConf {
        workspace: args.workspace.trim().to_string(),
        controller_address: args.controller.trim().to_string(),
        connector_address: String::new(),
        http_base_url: args.http_base.trim().to_string(),
    };
    match config::save(&conf) {
        Ok(path) => to_c(json!({ "ok": true, "path": path.display().to_string() })),
        Err(e) => error(e),
    }
}

/// Sends one daemon IPC request, e.g. `{"type":"Status"}`, `{"type":"Up"}`,
/// and returns its JSON response.
///
/// # Safety
/// `request` must be a NUL-terminated JSON IPC request.
#[no_mangle]
pub unsafe extern "C" fn zc_request(request: *const c_char) -> *mut c_char {
    let req: ipc::IpcRequest = match serde_json::from_str(&from_c(request)) {
        Ok(r) => r,
        Err(e) => return error(format!("bad request: {e}")),
    };
    match runtime().block_on(ipc::send_ipc(&req)) {
        Ok(resp) => match serde_json::to_value(&resp) {
            Ok(v) => to_c(v),
            Err(e) => error(e),
        },
        Err(e) => error(format!("{e:#}")),
    }
}

/// Starts `zecurity-client login` in the background. Poll with
/// `zc_login_poll` for the URL to open and the result.
#[no_mangle]
pub extern "C" fn zc_login_start() -> *mut c_char {
    {
        let mut s = match login_state().lock() {
            Ok(s) => s,
            Err(e) => return error(e),
        };
        if s.running {
            return error("a login is already in progress");
        }
        *s = LoginState {
            running: true,
            ..Default::default()
        };
    }
    runtime().spawn(async {
        let result = cmd::login::run().await.map_err(|e| format!("{e:#}"));
        if let Ok(mut s) = login_state().lock() {
            s.running = false;
            s.result = Some(result);
        }
    });
    to_c(json!({ "ok": true }))
}

/// `{"state": "idle" | "running" | "done" | "error", "url"?, "error"?}`.
/// The URL is handed out once, for the UI to open in the browser.
#[no_mangle]
pub extern "C" fn zc_login_poll() -> *mut c_char {
    let mut s = match login_state().lock() {
        Ok(s) => s,
        Err(e) => return error(e),
    };
    let url = s.url.take();
    let value = match (&s.result, s.running) {
        (_, true) => json!({ "ok": true, "state": "running", "url": url }),
        (Some(Ok(())), false) => json!({ "ok": true, "state": "done" }),
        (Some(Err(e)), false) => json!({ "ok": true, "state": "error", "error": e }),
        (None, false) => json!({ "ok": true, "state": "idle" }),
    };
    to_c(value)
}

/// Signs out (what `zecurity-client logout` does).
#[no_mangle]
pub extern "C" fn zc_logout() -> *mut c_char {
    match runtime().block_on(cmd::logout::run()) {
        Ok(()) => to_c(json!({ "ok": true })),
        Err(e) => error(format!("{e:#}")),
    }
}

// ── JNI: the VPN service process ─────────────────────────────────────────────

mod jni_service {
    use std::net::Ipv4Addr;
    use std::path::PathBuf;
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::sync::OnceLock;

    use anyhow::{anyhow, Context, Result};
    use jni::objects::{GlobalRef, JClass, JStaticMethodID, JString, JValue};
    use jni::signature::{Primitive, ReturnType};
    use jni::sys::{jint, JNI_VERSION_1_6};
    use jni::{JNIEnv, JavaVM};
    use zecurity_core::{android, daemon};

    static JVM: OnceLock<JavaVM> = OnceLock::new();
    static NATIVE_CLASS: OnceLock<GlobalRef> = OnceLock::new();
    static ESTABLISH: OnceLock<JStaticMethodID> = OnceLock::new();
    static DAEMON_RUNNING: AtomicBool = AtomicBool::new(false);

    /// Runs on `System.loadLibrary` in the service process.
    #[no_mangle]
    pub extern "system" fn JNI_OnLoad(vm: JavaVM, _reserved: *mut std::ffi::c_void) -> jint {
        let _ = JVM.set(vm);
        JNI_VERSION_1_6
    }

    /// `ZecurityNative.startDaemon(filesDir)`: starts the daemon once per
    /// process. Called on a Java thread, so the class and method can be
    /// looked up here and cached for the daemon's threads.
    #[no_mangle]
    pub extern "system" fn Java_io_zecurity_client_ZecurityNative_startDaemon(
        mut env: JNIEnv,
        class: JClass,
        files_dir: JString,
    ) -> jint {
        let dir: String = match env.get_string(&files_dir) {
            Ok(s) => s.into(),
            Err(_) => return fail(&mut env),
        };
        super::install_crypto_provider();
        android::init(PathBuf::from(dir));
        if NATIVE_CLASS.get().is_none() {
            match env.new_global_ref(&class) {
                Ok(g) => {
                    let _ = NATIVE_CLASS.set(g);
                }
                Err(_) => return fail(&mut env),
            }
        }
        if ESTABLISH.get().is_none() {
            match env.get_static_method_id(&class, "establish", "(Ljava/lang/String;)I") {
                Ok(m) => {
                    let _ = ESTABLISH.set(m);
                }
                Err(_) => return fail(&mut env),
            }
        }
        android::set_tun_provider(Box::new(establish_via_service));

        if DAEMON_RUNNING.swap(true, Ordering::SeqCst) {
            return 0;
        }
        std::thread::spawn(|| {
            let result = tokio::runtime::Builder::new_multi_thread()
                .enable_all()
                .build()
                .map_err(anyhow::Error::from)
                .and_then(|rt| rt.block_on(daemon::run()));
            if let Err(e) = result {
                // Not configured yet, or a startup failure: the daemon log
                // may not exist, so leave a marker the app can show.
                let _ = std::fs::write(
                    android::files_dir().join("daemon.error"),
                    format!("{e:#}"),
                );
            }
            DAEMON_RUNNING.store(false, Ordering::SeqCst);
        });
        0
    }

    /// A JNI error leaves a Java exception pending, which would be thrown into
    /// the service on return and crash its process: clear it and report -1.
    fn fail(env: &mut JNIEnv) -> jint {
        if env.exception_check().unwrap_or(false) {
            let _ = env.exception_describe();
            let _ = env.exception_clear();
        }
        -1
    }

    /// The daemon's TUN provider: `ZecurityNative.establish("ip,ip,...")`
    /// returns the VPN interface's file descriptor, or a negative value.
    fn establish_via_service(routes: &[Ipv4Addr]) -> Result<i32> {
        let vm = JVM.get().ok_or_else(|| anyhow!("JNI not initialised"))?;
        let class = NATIVE_CLASS.get().ok_or_else(|| anyhow!("JNI class not cached"))?;
        let method = *ESTABLISH.get().ok_or_else(|| anyhow!("JNI method not cached"))?;
        let mut env = vm.attach_current_thread().context("attach to the JVM")?;
        let list = routes
            .iter()
            .map(|ip| ip.to_string())
            .collect::<Vec<_>>()
            .join(",");
        let jlist = env.new_string(list).context("routes string")?;
        let class: &JClass = class.as_obj().into();
        // SAFETY: `method` was looked up on this class with signature
        // (Ljava/lang/String;)I, and the one argument is a String.
        let value = unsafe {
            env.call_static_method_unchecked(
                class,
                method,
                ReturnType::Primitive(Primitive::Int),
                &[JValue::Object(&jlist).as_jni()],
            )
        };
        if env.exception_check().unwrap_or(false) {
            let _ = env.exception_clear();
            return Err(anyhow!("the VPN service threw while creating the interface"));
        }
        value.context("call establish")?.i().context("establish result")
    }
}
