// The Linux collectors below are compiled, and unit-tested, on every
// platform; on Windows only `collect` (OS report only) runs.
#![cfg_attr(any(windows, target_os = "android", target_os = "ios"), allow(dead_code))]
use std::collections::HashMap;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;
use crate::grpc::client_v1::{CheckStatus, PostureCheck};

pub(crate) const CHECK_ID_OS_VERSION: &str = "linux.os.version";
pub(crate) const CHECK_ID_LUKS: &str = "linux.disk_encryption.luks";
pub(crate) const CHECK_ID_FIREWALL: &str = "linux.firewall.active";
pub(crate) const CHECK_ID_SECURE_BOOT: &str = "linux.secure_boot.enabled";

// ADR-031 D-4: Android checks, read by the app's VPN service (Kotlin).
// Identical in controller/internal/posture/validate.go.
pub(crate) const CHECK_ID_ANDROID_ENCRYPTION: &str = "android.disk_encryption.enabled";
pub(crate) const CHECK_ID_ANDROID_SCREEN_LOCK: &str = "android.screen_lock.enabled";
pub(crate) const CHECK_ID_ANDROID_BIOMETRIC: &str = "android.biometric.enrolled";
const ANDROID_CHECK_IDS: [&str; 3] = [
    CHECK_ID_ANDROID_ENCRYPTION,
    CHECK_ID_ANDROID_SCREEN_LOCK,
    CHECK_ID_ANDROID_BIOMETRIC,
];

/// Converts the checks a mobile platform read into report checks. Only the
/// known IDs, once each, are kept; an unknown status is reported as ERROR
/// (never as PASS); details are capped.
pub(crate) fn checks_from_platform(
    known: &[&str],
    raw: Vec<(String, String, String)>,
) -> Vec<PostureCheck> {
    let mut out: Vec<PostureCheck> = Vec::new();
    for (id, status, detail) in raw {
        if !known.contains(&id.as_str()) || out.iter().any(|c| c.check_id == id) {
            continue;
        }
        let status = match status.as_str() {
            "PASS" => CheckStatus::Pass,
            "FAIL" => CheckStatus::Fail,
            "UNSUPPORTED" => CheckStatus::Unsupported,
            _ => CheckStatus::Error,
        };
        let detail: String = detail.chars().take(200).collect();
        out.push(check(&id, status, detail));
    }
    out
}

const DEFAULT_TIMEOUT: Duration = Duration::from_secs(10);
const SECURE_BOOT_GUID: &str = "8be4df61-93ca-11d2-aa0d-00e098032b8c";

pub(crate) fn check(check_id: &str, status: CheckStatus, detail: impl Into<String>) -> PostureCheck {
    PostureCheck {
        check_id: check_id.into(),
        status: status as i32,
        detail: detail.into(),
    }
}

pub(crate) fn error_check(check_id: &str, detail: &str) -> PostureCheck {
    check(check_id, CheckStatus::Error, detail)
}

fn read_os_release(root: &Path) -> Option<HashMap<String, String>> {
    let primary = root.join("etc/os-release");
    let fallback = root.join("usr/lib/os-release");
    let contents = std::fs::read_to_string(&primary)
        .or_else(|_| std::fs::read_to_string(&fallback))
        .ok()?;

    let mut map = HashMap::new();
    for line in contents.lines() {
        if let Some((key, value)) = line.split_once('=') {
            map.insert(key.to_string(), value.trim_matches('"').to_string());
        }
    }
    Some(map)
}

pub(crate) fn detect_os_release(root: &Path) -> (String, String) {
    match read_os_release(root) {
        Some(kv) => {
            let name = kv.get("NAME").cloned().unwrap_or_else(|| "linux".into());
            let version = kv
                .get("VERSION_ID")
                .or_else(|| kv.get("VERSION"))
                .cloned()
                .unwrap_or_else(|| "unknown".into());
            (name, version)
        }
        None => ("linux".into(), "unknown".into()),
    }
}

pub(crate) fn collect_os_version_check(root: &Path) -> PostureCheck {
    let os_release_exists =
        root.join("etc/os-release").exists() || root.join("usr/lib/os-release").exists();

    match read_os_release(root) {
        Some(kv) if kv.contains_key("NAME") => {
            let name = &kv["NAME"];
            let version = kv.get("VERSION_ID").map(String::as_str).unwrap_or("?");
            check(
                CHECK_ID_OS_VERSION,
                CheckStatus::Pass,
                format!("{name} {version}"),
            )
        }
        Some(_) => check(
            CHECK_ID_OS_VERSION,
            CheckStatus::Fail,
            "os-release present but unparsable"
          
        ),
        None if os_release_exists => error_check(CHECK_ID_OS_VERSION, "failed to read os-release"),
        None => check(
            CHECK_ID_OS_VERSION,
            CheckStatus::Unknown,
            "no os-release file found",
        ),
    }
}

pub(crate) fn collect_luks(root: &Path) -> PostureCheck {
    let crypttab = root.join("etc/crypttab");
    let mapper_dir = root.join("dev/mapper");

    let text = match std::fs::read_to_string(&crypttab) {
        Ok(t) => t,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            return check(
                CHECK_ID_LUKS,
                CheckStatus::Fail,
                "no /etc/crypttab - LUKS not configured",
            );
        }
        Err(_) => return error_check(CHECK_ID_LUKS, "failed to read /etc/crypttab"),
    };

    if !mapper_dir.is_dir() {
        return check(
            CHECK_ID_LUKS,
            CheckStatus::Unknown,
            "/dev/mapper not present - cannot verify",
        );
    }

    let entries: Vec<&str> = text
        .lines()
        .map(str::trim)
        .filter(|l| !l.is_empty() && !l.starts_with('#'))
        .collect();

    if entries.is_empty() {
        return check(CHECK_ID_LUKS, CheckStatus::Fail, "crypttab has no entries");
    }

    let all_unlocked = entries.iter().all(|line| {
        line.split_whitespace()
            .next()
            .map(|name| mapper_dir.join(name).exists())
            .unwrap_or(false)
    });

    if all_unlocked {
        check(
            CHECK_ID_LUKS,
            CheckStatus::Pass,
            format!("{} LUKS volume(s) unlocked", entries.len()),
        )
    } else {
        check(
            CHECK_ID_LUKS,
            CheckStatus::Fail,
            "one or more LUKS volumes not unlocked",
        )
    }
}

pub(crate) fn collect_secure_boot(root: &Path) -> PostureCheck {
    if !root.join("sys/firmware/efi").is_dir() {
        return check(
            CHECK_ID_SECURE_BOOT,
            CheckStatus::Unsupported,
            "legacy BIOS, no UEFI",
        );
    }

    let var_name = format!("SecureBoot-{SECURE_BOOT_GUID}");
    let var_path = root.join("sys/firmware/efi/efivars").join(var_name);

    match std::fs::read(&var_path) {
        Ok(bytes) if bytes.len() >= 5 && bytes[4] == 1 => {
            check(CHECK_ID_SECURE_BOOT, CheckStatus::Pass, "Secure Boot enabled")
        }
        Ok(bytes) if bytes.len() >= 5 && bytes[4] == 0 => {
            check(CHECK_ID_SECURE_BOOT, CheckStatus::Fail, "Secure Boot disabled")
        }
        Ok(_) => check(CHECK_ID_SECURE_BOOT, CheckStatus::Unknown, "efivar malformed"),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            check(CHECK_ID_SECURE_BOOT, CheckStatus::Unknown, "efivar not exposed")
        }
        Err(_) => error_check(CHECK_ID_SECURE_BOOT, "failed to read efivar (permissions?)"),
    }
}

#[async_trait::async_trait]
pub(crate) trait CommandRunner: Send + Sync {
    async fn run(&self, program: &str, args: &[&str]) -> std::io::Result<std::process::Output>;
}

pub(crate) struct RealCommandRunner;

#[async_trait::async_trait]
impl CommandRunner for RealCommandRunner {
    async fn run(&self, program: &str, args: &[&str]) -> std::io::Result<std::process::Output> {
        tokio::process::Command::new(program)
            .args(args)
            .kill_on_drop(true)
            .output()
            .await
    }
}

pub(crate) async fn try_backend(
    runner: &dyn CommandRunner,
    program: &str,
    args: &[&str],
    pass_pattern: &[&str],
    fail_msg: &str,
) -> Option<PostureCheck> {
    match runner.run(program, args).await {
        Ok(out) => {
            let text = String::from_utf8_lossy(&out.stdout);
            let result = if pass_pattern.iter().any(|p| text.contains(p)) {
                check(
                    CHECK_ID_FIREWALL,
                    CheckStatus::Pass,
                    format!("{program} default-deny active"),
                )
            } else if out.status.success() {
                check(CHECK_ID_FIREWALL, CheckStatus::Fail, fail_msg)
            } else {
                error_check(CHECK_ID_FIREWALL, &format!("{program} exited with error"))
            };
            Some(result)
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => None,
        Err(_) => Some(error_check(
            CHECK_ID_FIREWALL,
            &format!("{program} failed to execute"),
        )),
    }
}

/// nftables: default-deny means an IPv4 input-hook base chain (family `inet`
/// or `ip`) whose policy is drop or reject. Read from the JSON ruleset, not
/// the text: "policy drop" anywhere in the text also matched a forward or
/// output chain (e.g. Docker's), passing a host whose inbound was wide open.
/// With iptables-nft, iptables' own INPUT chain is part of this ruleset too.
pub(crate) async fn nft_input_default_deny(runner: &dyn CommandRunner) -> Option<PostureCheck> {
    let out = match runner.run("nft", &["-j", "list", "ruleset"]).await {
        Ok(out) => out,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return None,
        Err(_) => return Some(error_check(CHECK_ID_FIREWALL, "nft failed to execute")),
    };
    if !out.status.success() {
        return Some(error_check(CHECK_ID_FIREWALL, "nft exited with error"));
    }
    let Ok(ruleset) = serde_json::from_slice::<serde_json::Value>(&out.stdout) else {
        return Some(error_check(CHECK_ID_FIREWALL, "nft ruleset is not valid JSON"));
    };
    let deny = ruleset
        .get("nftables")
        .and_then(|v| v.as_array())
        .into_iter()
        .flatten()
        .filter_map(|item| item.get("chain"))
        .find(|chain| {
            let field = |k: &str| chain.get(k).and_then(|v| v.as_str()).unwrap_or("");
            field("hook") == "input"
                && matches!(field("family"), "inet" | "ip")
                && matches!(field("policy"), "drop" | "reject")
        });
    Some(match deny {
        Some(chain) => {
            let field = |k: &str| chain.get(k).and_then(|v| v.as_str()).unwrap_or("?");
            check(
                CHECK_ID_FIREWALL,
                CheckStatus::Pass,
                format!(
                    "nft input chain {} {}/{} policy {}",
                    field("family"),
                    field("table"),
                    field("name"),
                    field("policy")
                ),
            )
        }
        None => check(
            CHECK_ID_FIREWALL,
            CheckStatus::Fail,
            "nftables present but no default-deny input chain",
        ),
    })
}

/// True when `iptables --version` reports the legacy (xtables) backend. Its
/// rules, ufw's included, are not part of the nftables ruleset. Needs no
/// privilege, unlike listing the rules.
pub(crate) async fn iptables_is_legacy(runner: &dyn CommandRunner) -> bool {
    match runner.run("iptables", &["--version"]).await {
        Ok(out) if out.status.success() => {
            String::from_utf8_lossy(&out.stdout).contains("(legacy)")
        }
        _ => false,
    }
}

fn read_shell_vars(path: &Path) -> Option<HashMap<String, String>> {
    let contents = std::fs::read_to_string(path).ok()?;
    let mut map = HashMap::new();
    for line in contents.lines() {
        let line = line.trim();
        if line.starts_with('#') {
            continue;
        }
        if let Some((key, value)) = line.split_once('=') {
            map.insert(
                key.trim().to_string(),
                value.trim().trim_matches('"').to_string(),
            );
        }
    }
    Some(map)
}

/// systemd's state of `ufw.service` (`active`, `inactive`, `failed`, ...).
/// The unit is what loads ufw's rules at boot, so ufw.conf saying
/// `ENABLED=yes` proves nothing while it isn't active: found live 2026-11-05
/// on a host whose ufw.service was disabled, with INPUT policy ACCEPT and the
/// check reporting PASS. `systemctl is-active` needs no privilege; it prints
/// the state and exits non-zero for anything but active. None when systemctl
/// can't be run or prints nothing.
pub(crate) async fn ufw_service_state(runner: &dyn CommandRunner) -> Option<String> {
    let out = runner
        .run("systemctl", &["is-active", "ufw.service"])
        .await
        .ok()?;
    let state = String::from_utf8_lossy(&out.stdout).trim().to_string();
    (!state.is_empty()).then_some(state)
}

/// ufw's own configuration, for an iptables-legacy host whose rules the
/// daemon can't read (`iptables` needs root's xtables lock, `ufw status`
/// needs root, and `iptables-save` needs CAP_NET_RAW as well). It passes only
/// when ufw.conf says enabled, the default input policy is DROP or REJECT, and
/// `service` (see [`ufw_service_state`]) is `active`, i.e. the rules were
/// loaded. A manual `iptables -P INPUT ACCEPT` after that is still not seen.
/// `ufw enable` runs ufw-init itself and never starts the unit, so with the
/// unit inactive (e.g. disabled) an enabled ufw fails closed until
/// `systemctl enable --now ufw`: no unprivileged signal shows its rules.
/// None when ufw isn't configured on the host.
pub(crate) fn ufw_config_default_deny(root: &Path, service: Option<&str>) -> Option<PostureCheck> {
    let conf = read_shell_vars(&root.join("etc/ufw/ufw.conf"))?;
    let enabled = conf.get("ENABLED").map(String::as_str) == Some("yes");
    if !enabled {
        return Some(check(
            CHECK_ID_FIREWALL,
            CheckStatus::Fail,
            "ufw disabled (/etc/ufw/ufw.conf)",
        ));
    }
    let policy = read_shell_vars(&root.join("etc/default/ufw"))
        .and_then(|d| d.get("DEFAULT_INPUT_POLICY").cloned())
        .unwrap_or_default();
    Some(match (policy.as_str(), service) {
        ("DROP" | "REJECT", Some("active")) => check(
            CHECK_ID_FIREWALL,
            CheckStatus::Pass,
            format!(
                "ufw enabled per /etc/ufw/ufw.conf, default input policy {policy}, \
                 ufw.service active (iptables-legacy: runtime rules not readable)"
            ),
        ),
        ("DROP" | "REJECT", Some(state)) => check(
            CHECK_ID_FIREWALL,
            CheckStatus::Fail,
            format!(
                "ufw enabled in /etc/ufw/ufw.conf but ufw.service is {state}, so its \
                 rules can't be confirmed as loaded (fix: systemctl enable --now ufw)"
            ),
        ),
        ("DROP" | "REJECT", None) => check(
            CHECK_ID_FIREWALL,
            CheckStatus::Fail,
            "ufw enabled in /etc/ufw/ufw.conf but whether ufw.service loaded its \
             rules can't be checked (systemctl unavailable)",
        ),
        _ => check(
            CHECK_ID_FIREWALL,
            CheckStatus::Fail,
            format!("ufw enabled but default input policy is {policy:?}"),
        ),
    })
}

pub(crate) async fn collect_firewall(runner: Arc<dyn CommandRunner>, root: &Path) -> PostureCheck {
    let nft = nft_input_default_deny(runner.as_ref()).await;
    if let Some(c) = &nft {
        if c.status == CheckStatus::Pass as i32 {
            return c.clone();
        }
    }
    // With iptables-nft (or no iptables) the nftables ruleset holds every rule,
    // so its answer stands. With iptables-legacy it doesn't: look further.
    let legacy = iptables_is_legacy(runner.as_ref()).await;
    if let Some(c) = nft.as_ref().filter(|_| !legacy) {
        return c.clone();
    }

    let iptables = try_backend(
        runner.as_ref(),
        "iptables",
        &["-L", "-n"],
        &["Chain INPUT (policy DROP)", "Chain INPUT (policy REJECT)"],
        "iptables present but not default-deny",
    )
    .await;
    match iptables {
        // Unreadable rules on a legacy host (not root) settle nothing.
        Some(c) if c.status == CheckStatus::Error as i32 && legacy => {}
        Some(c) => return c,
        None => {}
    }

    match runner.run("ufw", &["status"]).await {
        Ok(out) if out.status.success() => {
            let text = String::from_utf8_lossy(&out.stdout);
            return if text.contains("Status: active") {
                check(CHECK_ID_FIREWALL, CheckStatus::Pass, "ufw active")
            } else {
                check(CHECK_ID_FIREWALL, CheckStatus::Fail, "ufw inactive")
            };
        }
        // Not root: `ufw status` refuses, which says nothing about the firewall.
        Ok(_) => {}
        Err(_) => {}
    }

    if legacy {
        let service = ufw_service_state(runner.as_ref()).await;
        if let Some(c) = ufw_config_default_deny(root, service.as_deref()) {
            return c;
        }
    }
    nft.unwrap_or_else(|| {
        check(
            CHECK_ID_FIREWALL,
            CheckStatus::Fail,
            "no firewall backend found (nft/iptables/ufw)",
        )
    })
}

pub(crate) async fn run_check(
    check_id: &'static str,
    timeout: Duration,
    f: impl FnOnce() -> PostureCheck + Send + 'static,
) -> PostureCheck {
    match tokio::time::timeout(timeout, tokio::task::spawn_blocking(f)).await {
        Ok(Ok(result)) => result,
        Ok(Err(join_err)) => {
            let detail = if join_err.is_panic() {
                "collector panicked"
            } else {
                "collector task was cancelled"
            };
            error_check(check_id, detail)
        }
        Err(_elapsed) => error_check(
            check_id,
            "collector timed out (file I/O can't be forcibly cancelled)",
        ),
    }
}

pub(crate) async fn run_async_check<F>(check_id: &'static str, timeout: Duration, fut: F) -> PostureCheck
where
    F: std::future::Future<Output = PostureCheck> + Send + 'static,
{
    let handle = tokio::spawn(fut);
    let abort = handle.abort_handle();

    match tokio::time::timeout(timeout, handle).await {
        Ok(Ok(result)) => result,
        Ok(Err(join_err)) => {
            let detail = if join_err.is_panic() {
                "collector panicked"
            } else {
                "collector task was cancelled"
            };
            error_check(check_id, detail)
        }
        Err(_elapsed) => {
            abort.abort();
            error_check(check_id, "collector timed out")
        }
    }
}

pub(crate) struct PostureReport {
    pub checks: Vec<PostureCheck>,
    pub os_name: String,
    pub os_version: String,
}

#[cfg(not(any(windows, target_os = "android", target_os = "ios")))]
pub(crate) async fn collect() -> PostureReport {
    collect_with(Path::new("/"), Arc::new(RealCommandRunner), DEFAULT_TIMEOUT).await
}

/// Windows: the `linux.*` collectors don't apply and `windows.*` checks are
/// not built yet, so only the OS is reported. A posture profile that requires
/// a check evaluates it as unsupported on the controller (fail closed unless
/// the profile sets allow_unsupported).
#[cfg(windows)]
pub(crate) async fn collect() -> PostureReport {
    let version = match tokio::time::timeout(
        DEFAULT_TIMEOUT,
        RealCommandRunner.run("cmd", &["/C", "ver"]),
    )
    .await
    {
        Ok(Ok(out)) if out.status.success() => windows_version_from_ver(&String::from_utf8_lossy(&out.stdout)),
        _ => "unknown".into(),
    };
    PostureReport {
        checks: Vec::new(),
        os_name: "Windows".into(),
        os_version: version,
    }
}

/// Mobile: the OS, plus (Android, ADR-031) the checks the app's VPN service
/// reads: storage encryption, screen lock, strong biometric enrolled. iOS
/// has no checks yet. Android reads its version with `getprop`; on iOS the
/// app supplies it (`UIDevice.systemVersion`).
#[cfg(any(target_os = "android", target_os = "ios"))]
pub(crate) async fn collect() -> PostureReport {
    #[cfg(target_os = "android")]
    let (os_name, version) = (
        "Android",
        match tokio::time::timeout(
            DEFAULT_TIMEOUT,
            RealCommandRunner.run("getprop", &["ro.build.version.release"]),
        )
        .await
        {
            Ok(Ok(out)) if out.status.success() => {
                Some(String::from_utf8_lossy(&out.stdout).trim().to_string())
            }
            _ => None,
        },
    );
    #[cfg(target_os = "ios")]
    let (os_name, version) = ("iOS", crate::mobile::os_version());
    // The provider calls into the JVM: off the async workers.
    let raw = tokio::task::spawn_blocking(crate::mobile::platform_posture)
        .await
        .unwrap_or_default();
    #[cfg(target_os = "android")]
    let checks = checks_from_platform(&ANDROID_CHECK_IDS, raw);
    #[cfg(target_os = "ios")]
    let checks = checks_from_platform(&[], raw);
    PostureReport {
        checks,
        os_name: os_name.into(),
        os_version: version
            .filter(|v| !v.is_empty())
            .unwrap_or_else(|| "unknown".into()),
    }
}

/// `ver` prints e.g. "Microsoft Windows [Version 10.0.22631.4317]".
#[cfg_attr(not(windows), allow(dead_code))]
pub(crate) fn windows_version_from_ver(output: &str) -> String {
    output
        .split("[Version ")
        .nth(1)
        .and_then(|rest| rest.split(']').next())
        .map(|v| v.trim().to_string())
        .filter(|v| !v.is_empty())
        .unwrap_or_else(|| "unknown".into())
}

pub(crate) async fn collect_with(
    root: &Path,
    runner: Arc<dyn CommandRunner>,
    timeout: Duration,
) -> PostureReport {
    let (os_name, os_version) = detect_os_release(root);

    let root_a = root.to_path_buf();
    let root_b = root.to_path_buf();
    let root_c = root.to_path_buf();
    let root_d = root.to_path_buf();

    let (os_check, luks_check, fw_check, sb_check) = tokio::join!(
        run_check(CHECK_ID_OS_VERSION, timeout, move || {
            collect_os_version_check(&root_a)
        }),
        run_check(CHECK_ID_LUKS, timeout, move || collect_luks(&root_b)),
        run_async_check(CHECK_ID_FIREWALL, timeout, async move {
            collect_firewall(runner, &root_d).await
        }),
        run_check(CHECK_ID_SECURE_BOOT, timeout, move || {
            collect_secure_boot(&root_c)
        }),
    );

    PostureReport {
        checks: vec![os_check, luks_check, fw_check, sb_check],
        os_name,
        os_version,
    }
}

#[cfg(test)]
mod windows_ver_tests {
    #[test]
    fn windows_version_is_parsed_from_ver_output() {
        use crate::posture::windows_version_from_ver;
        assert_eq!(
            windows_version_from_ver("\r\nMicrosoft Windows [Version 10.0.22631.4317]\r\n"),
            "10.0.22631.4317"
        );
        assert_eq!(windows_version_from_ver("garbage"), "unknown");
        assert_eq!(windows_version_from_ver("Microsoft Windows [Version ]"), "unknown");
    }
}

#[cfg(test)]
mod platform_check_tests {
    use super::*;

    fn raw(id: &str, status: &str, detail: &str) -> (String, String, String) {
        (id.into(), status.into(), detail.into())
    }

    #[test]
    fn android_checks_are_converted_and_filtered() {
        let got = checks_from_platform(
            &ANDROID_CHECK_IDS,
            vec![
                raw(CHECK_ID_ANDROID_ENCRYPTION, "PASS", "file-based encryption active"),
                raw(CHECK_ID_ANDROID_SCREEN_LOCK, "FAIL", "no screen lock"),
                raw(CHECK_ID_ANDROID_BIOMETRIC, "UNSUPPORTED", "needs Android 11"),
                raw("linux.firewall.active", "PASS", "not an Android check"),
                raw(CHECK_ID_ANDROID_SCREEN_LOCK, "PASS", "duplicate is ignored"),
            ],
        );
        let summary: Vec<(String, i32)> =
            got.iter().map(|c| (c.check_id.clone(), c.status)).collect();
        assert_eq!(
            summary,
            vec![
                (CHECK_ID_ANDROID_ENCRYPTION.to_string(), CheckStatus::Pass as i32),
                (CHECK_ID_ANDROID_SCREEN_LOCK.to_string(), CheckStatus::Fail as i32),
                (CHECK_ID_ANDROID_BIOMETRIC.to_string(), CheckStatus::Unsupported as i32),
            ]
        );
    }

    #[test]
    fn an_unknown_status_is_an_error_never_a_pass() {
        let got = checks_from_platform(
            &ANDROID_CHECK_IDS,
            vec![raw(CHECK_ID_ANDROID_SCREEN_LOCK, "pass", "lower-case is not PASS")],
        );
        assert_eq!(got[0].status, CheckStatus::Error as i32);
    }

    #[test]
    fn details_are_capped() {
        let got = checks_from_platform(
            &ANDROID_CHECK_IDS,
            vec![raw(CHECK_ID_ANDROID_ENCRYPTION, "PASS", &"x".repeat(1000))],
        );
        assert_eq!(got[0].detail.len(), 200);
    }
}

