use std::collections::HashMap;
use std::io;
use std::os::unix::process::ExitStatusExt;
use std::path::Path;
use std::process::{ExitStatus, Output};
use std::sync::Arc;
use std::time::Duration;

use tempfile::TempDir;

use crate::grpc::client_v1::CheckStatus;
use crate::posture::*;

fn write(dir: &Path, rel: &str, contents: &[u8]) {
    let path = dir.join(rel);
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(path, contents).unwrap();
}

fn running_as_root() -> bool {
    unsafe { libc::geteuid() == 0 }
}

fn make_output(code: i32, stdout: &str) -> io::Result<Output> {
    Ok(Output {
        status: ExitStatus::from_raw(code << 8),
        stdout: stdout.as_bytes().to_vec(),
        stderr: Vec::new(),
    })
}

// ---------- os_version ----------

#[test]
fn os_version_pass_when_parseable() {
    let tmp = TempDir::new().unwrap();
    write(tmp.path(), "etc/os-release", b"NAME=\"Ubuntu\"\nVERSION_ID=\"24.04\"\n");

    let (name, version) = detect_os_release(tmp.path());
    assert_eq!((name.as_str(), version.as_str()), ("Ubuntu", "24.04"));

    let result = collect_os_version_check(tmp.path());
    assert_eq!(result.status, CheckStatus::Pass as i32);
}

#[test]
fn os_version_fail_when_unparseable() {
    let tmp = TempDir::new().unwrap();
    write(tmp.path(), "etc/os-release", b"not a key value file at all");

    let result = collect_os_version_check(tmp.path());
    assert_eq!(result.status, CheckStatus::Fail as i32);
}

#[test]
fn os_version_unknown_when_no_file() {
    let tmp = TempDir::new().unwrap();

    let result = collect_os_version_check(tmp.path());
    assert_eq!(result.status, CheckStatus::Unknown as i32);
}

// ---------- LUKS ----------

#[test]
fn luks_pass_when_all_mapped() {
    let tmp = TempDir::new().unwrap();
    write(tmp.path(), "etc/crypttab", b"cryptroot /dev/sda2 none luks\n");
    write(tmp.path(), "dev/mapper/cryptroot", b"");

    let result = collect_luks(tmp.path());
    assert_eq!(result.status, CheckStatus::Pass as i32);
}

#[test]
fn luks_fail_when_no_crypttab() {
    let tmp = TempDir::new().unwrap();
    std::fs::create_dir_all(tmp.path().join("dev/mapper")).unwrap();

    let result = collect_luks(tmp.path());
    assert_eq!(result.status, CheckStatus::Fail as i32);
}

#[test]
fn luks_fail_when_entry_not_unlocked() {
    let tmp = TempDir::new().unwrap();
    write(tmp.path(), "etc/crypttab", b"cryptroot /dev/sda2 none luks\n");
    std::fs::create_dir_all(tmp.path().join("dev/mapper")).unwrap();
    // no matching mapper device created — entry configured but not unlocked

    let result = collect_luks(tmp.path());
    assert_eq!(result.status, CheckStatus::Fail as i32);
}

#[test]
fn luks_unknown_when_no_mapper_dir() {
    let tmp = TempDir::new().unwrap();
    write(tmp.path(), "etc/crypttab", b"cryptroot /dev/sda2 none luks\n");
    // /dev/mapper itself doesn't exist at all

    let result = collect_luks(tmp.path());
    assert_eq!(result.status, CheckStatus::Unknown as i32);
}

// ---------- Secure Boot ----------

#[test]
fn secure_boot_unsupported_on_legacy_bios() {
    let tmp = TempDir::new().unwrap();
    // no sys/firmware/efi directory at all

    let result = collect_secure_boot(tmp.path());
    assert_eq!(result.status, CheckStatus::Unsupported as i32);
}

#[test]
fn secure_boot_pass_when_enabled() {
    let tmp = TempDir::new().unwrap();
    std::fs::create_dir_all(tmp.path().join("sys/firmware/efi")).unwrap();
    let var = "sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c";
    write(tmp.path(), var, &[0, 0, 0, 0, 1]);

    let result = collect_secure_boot(tmp.path());
    assert_eq!(result.status, CheckStatus::Pass as i32);
}

#[test]
fn secure_boot_fail_when_disabled() {
    let tmp = TempDir::new().unwrap();
    std::fs::create_dir_all(tmp.path().join("sys/firmware/efi")).unwrap();
    let var = "sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c";
    write(tmp.path(), var, &[0, 0, 0, 0, 0]);

    let result = collect_secure_boot(tmp.path());
    assert_eq!(result.status, CheckStatus::Fail as i32);
}

#[test]
fn secure_boot_unknown_when_efivar_missing() {
    let tmp = TempDir::new().unwrap();
    std::fs::create_dir_all(tmp.path().join("sys/firmware/efi/efivars")).unwrap();
    // efi dir exists, but the specific SecureBoot-* var file doesn't

    let result = collect_secure_boot(tmp.path());
    assert_eq!(result.status, CheckStatus::Unknown as i32);
}

// ---------- Firewall (fake subprocess runner) ----------

struct FakeCommandRunner {
    responses: HashMap<String, io::Result<Output>>,
}

impl FakeCommandRunner {
    fn key(program: &str, args: &[&str]) -> String {
        format!("{program} {}", args.join(" "))
    }
}

#[async_trait::async_trait]
impl CommandRunner for FakeCommandRunner {
    async fn run(&self, program: &str, args: &[&str]) -> io::Result<Output> {
        match self.responses.get(&Self::key(program, args)) {
            Some(Ok(out)) => Ok(Output {
                status: out.status,
                stdout: out.stdout.clone(),
                stderr: out.stderr.clone(),
            }),
            Some(Err(e)) => Err(io::Error::new(e.kind(), e.to_string())),
            None => Err(io::Error::from(io::ErrorKind::NotFound)),
        }
    }
}

#[tokio::test]
async fn firewall_pass_via_nft_default_deny() {
    let mut responses = HashMap::new();
    responses.insert(
        FakeCommandRunner::key("nft", &["list", "ruleset"]),
        make_output(0, "table inet filter {\n chain input {\n  policy drop\n }\n}"),
    );
    let runner = Arc::new(FakeCommandRunner { responses });

    let result = collect_firewall(runner).await;
    assert_eq!(result.status, CheckStatus::Pass as i32);
}

#[tokio::test]
async fn firewall_fail_when_nft_present_but_permissive() {
    let mut responses = HashMap::new();
    responses.insert(
        FakeCommandRunner::key("nft", &["list", "ruleset"]),
        make_output(0, "table inet filter {\n chain input {\n  policy accept\n }\n}"),
    );
    let runner = Arc::new(FakeCommandRunner { responses });

    let result = collect_firewall(runner).await;
    assert_eq!(result.status, CheckStatus::Fail as i32);
}

#[tokio::test]
async fn firewall_falls_through_to_iptables_when_nft_absent() {
    let mut responses = HashMap::new();
    responses.insert(
        FakeCommandRunner::key("iptables", &["-L", "-n"]),
        make_output(0, "Chain INPUT (policy DROP)\n"),
    );
    let runner = Arc::new(FakeCommandRunner { responses });
    // nft is intentionally absent from `responses` -> FakeCommandRunner returns NotFound for it

    let result = collect_firewall(runner).await;
    assert_eq!(result.status, CheckStatus::Pass as i32);
}

#[tokio::test]
async fn firewall_fail_when_no_backend_found() {
    let runner = Arc::new(FakeCommandRunner { responses: HashMap::new() });
    // nft, iptables, and ufw all return NotFound

    let result = collect_firewall(runner).await;
    assert_eq!(result.status, CheckStatus::Fail as i32);
}

#[tokio::test]
async fn firewall_error_when_nft_execution_fails() {
    let mut responses = HashMap::new();
    responses.insert(
        FakeCommandRunner::key("nft", &["list", "ruleset"]),
        make_output(1, ""),
    );
    let runner = Arc::new(FakeCommandRunner { responses });

    let result = collect_firewall(runner).await;
    assert_eq!(result.status, CheckStatus::Error as i32);
}

// ---------- Isolation wrapper: panic / timeout ----------

#[tokio::test]
async fn run_check_catches_panic_as_error() {
    let result = run_check("test.panic", Duration::from_secs(5), || panic!("boom")).await;
    assert_eq!(result.status, CheckStatus::Error as i32);
    assert!(result.detail.contains("panicked"));
}

#[tokio::test]
async fn run_check_catches_timeout_without_blocking_caller() {
    let start = tokio::time::Instant::now();

    let result = run_check("test.timeout", Duration::from_millis(50), || {
        std::thread::sleep(Duration::from_secs(60));
        check("test.timeout", CheckStatus::Pass, "should never get here")
    })
    .await;

    assert!(start.elapsed() < Duration::from_secs(2));
    assert_eq!(result.status, CheckStatus::Error as i32);
    assert!(result.detail.contains("timed out"));
}

#[tokio::test]
async fn run_async_check_catches_panic_as_error() {
    let result = run_async_check("test.panic", Duration::from_secs(5), async {
        panic!("boom")
    })
    .await;
    assert_eq!(result.status, CheckStatus::Error as i32);
    assert!(result.detail.contains("panicked"));
}

// This is the test that validates the actual multi-agent-review fix: a hung
// subprocess must be genuinely killed on timeout, not just detached.
#[tokio::test]
async fn firewall_subprocess_is_actually_killed_on_timeout() {
    let runner: Arc<dyn CommandRunner> = Arc::new(RealCommandRunner);
    let start = tokio::time::Instant::now();

    let hung = async move {
        // `sleep 999` stands in for a hung nft/iptables call.
        let _ = runner.run("sleep", &["999"]).await;
        check("test.hang", CheckStatus::Unknown, "should not reach here")
    };

    let result = run_async_check("test.hang", Duration::from_millis(200), hung).await;

    // If the subprocess were only detached (not killed), this test would still
    // return promptly (abort() unblocks the *waiter* either way) — the real
    // proof is that the process is gone, not just that we stopped waiting.
    assert!(start.elapsed() < Duration::from_secs(2));
    assert_eq!(result.status, CheckStatus::Error as i32);

    // Give the kill signal a moment to land, then confirm no `sleep 999` survives.
    tokio::time::sleep(Duration::from_millis(200)).await;
    let leftover = RealCommandRunner
        .run("pgrep", &["-f", "sleep 999"])
        .await
        .map(|o| !o.stdout.is_empty())
        .unwrap_or(false);
    assert!(!leftover, "hung `sleep 999` subprocess was not killed on timeout");
}

// ---------- collect_with end-to-end ----------

#[tokio::test]
async fn collect_with_returns_all_four_checks() {
    let tmp = TempDir::new().unwrap();
    write(tmp.path(), "etc/os-release", b"NAME=\"TestOS\"\nVERSION_ID=\"1.0\"\n");

    let runner = Arc::new(FakeCommandRunner { responses: HashMap::new() });
    let report = collect_with(tmp.path(), runner, Duration::from_secs(5)).await;

    assert_eq!(report.checks.len(), 4);
    assert_eq!(report.os_name, "TestOS");
    assert_eq!(report.os_version, "1.0");
}