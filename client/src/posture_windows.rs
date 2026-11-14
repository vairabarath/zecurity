//! Windows posture checks (ADR-031 amendment 2026-11-14): HD Encryption
//! (BitLocker), Screen Lock (Windows Hello PIN, no automatic sign-in),
//! Biometric (Windows Hello face or fingerprint), Firewall and Antivirus.
//!
//! The daemon runs as LocalSystem, so one PowerShell script reads them all
//! (WMI/CIM and the registry) and prints one `ZC|<id>|<STATUS>|<detail>`
//! line per check. The per-user checks (Screen Lock, Biometric) are read for
//! the enrolling user, whose SID the service command line carries.
//!
//! The script, its encoding and the parser are plain functions, unit-tested
//! on every platform; only `collect` (in posture.rs) runs it, on Windows.

use base64::Engine;

use crate::grpc::client_v1::PostureCheck;
use crate::posture::{checks_from_platform, error_check};

// Identical in controller/internal/posture/validate.go.
pub(crate) const CHECK_ID_WINDOWS_ENCRYPTION: &str = "windows.disk_encryption.bitlocker";
pub(crate) const CHECK_ID_WINDOWS_SCREEN_LOCK: &str = "windows.screen_lock.enabled";
pub(crate) const CHECK_ID_WINDOWS_BIOMETRIC: &str = "windows.biometric.enrolled";
pub(crate) const CHECK_ID_WINDOWS_FIREWALL: &str = "windows.firewall.enabled";
pub(crate) const CHECK_ID_WINDOWS_ANTIVIRUS: &str = "windows.antivirus.enabled";

pub(crate) const WINDOWS_CHECK_IDS: [&str; 5] = [
    CHECK_ID_WINDOWS_ENCRYPTION,
    CHECK_ID_WINDOWS_SCREEN_LOCK,
    CHECK_ID_WINDOWS_BIOMETRIC,
    CHECK_ID_WINDOWS_FIREWALL,
    CHECK_ID_WINDOWS_ANTIVIRUS,
];

/// The Windows Hello PIN credential provider. It keeps a subkey per user who
/// has a PIN, named by the user's SID.
const PIN_PROVIDER_KEY: &str = r"HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Authentication\Credential Providers\{D6886603-9D2F-4EB2-B667-1971041FA96B}";

/// The script's body; `__SID__` is replaced by the enrolling user's SID, or
/// by an empty string when it is unknown (a daemon started from a console).
const SCRIPT: &str = r#"
$ErrorActionPreference = 'Stop'
# Not `R`: that is PowerShell's alias for Invoke-History, and aliases win.
function Out-ZcCheck([string]$id, [string]$st, [string]$d) { Write-Output ('ZC|{0}|{1}|{2}' -f $id, $st, ($d -replace '[\r\n|]', ' ')) }
$sid = '__SID__'
$drive = $env:SystemDrive

try {
  $v = Get-CimInstance -Namespace root\cimv2\Security\MicrosoftVolumeEncryption -ClassName Win32_EncryptableVolume -Filter "DriveLetter='$drive'"
  if (-not $v) { Out-ZcCheck '__ENC__' 'UNSUPPORTED' "no BitLocker volume for $drive" }
  elseif ($v.ProtectionStatus -eq 1) { Out-ZcCheck '__ENC__' 'PASS' "BitLocker protection on ($drive)" }
  elseif ($v.ProtectionStatus -eq 0) { Out-ZcCheck '__ENC__' 'FAIL' "BitLocker protection off ($drive)" }
  else { Out-ZcCheck '__ENC__' 'FAIL' "BitLocker protection state unknown ($drive)" }
} catch {
  if ("$($_.Exception.Message)" -match 'namespace') { Out-ZcCheck '__ENC__' 'UNSUPPORTED' 'BitLocker is not available on this Windows edition' }
  else { Out-ZcCheck '__ENC__' 'ERROR' "$($_.Exception.Message)" }
}

try {
  $wl = Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon'
  if ("$($wl.AutoAdminLogon)" -eq '1') { Out-ZcCheck '__LOCK__' 'FAIL' 'automatic sign-in is on' }
  elseif (-not $sid) { Out-ZcCheck '__LOCK__' 'UNSUPPORTED' 'enrolling user unknown' }
  elseif (Test-Path -LiteralPath ('__PIN__\' + $sid)) { Out-ZcCheck '__LOCK__' 'PASS' 'Windows Hello PIN set; automatic sign-in off' }
  else { Out-ZcCheck '__LOCK__' 'FAIL' 'no Windows Hello PIN for the enrolled user' }
} catch { Out-ZcCheck '__LOCK__' 'ERROR' "$($_.Exception.Message)" }

try {
  $f = $null
  if ($sid) { $f = (Get-ItemProperty -LiteralPath ('HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\WinBio\AccountInfo\' + $sid) -ErrorAction SilentlyContinue).EnrolledFactors }
  $sensors = @(Get-PnpDevice -Class Biometric -PresentOnly -ErrorAction SilentlyContinue | Where-Object { $_.Status -eq 'OK' })
  if ($f -and [int]$f -ne 0) { Out-ZcCheck '__BIO__' 'PASS' ('Windows Hello biometric enrolled (factors 0x{0:X})' -f [int]$f) }
  elseif ($sensors.Count -eq 0) { Out-ZcCheck '__BIO__' 'UNSUPPORTED' 'no biometric sensor' }
  elseif (-not $sid) { Out-ZcCheck '__BIO__' 'UNSUPPORTED' 'enrolling user unknown' }
  else { Out-ZcCheck '__BIO__' 'FAIL' 'no Windows Hello face or fingerprint enrolled' }
} catch { Out-ZcCheck '__BIO__' 'ERROR' "$($_.Exception.Message)" }

try {
  $off = @(Get-NetFirewallProfile | Where-Object { "$($_.Enabled)" -ne 'True' } | ForEach-Object { $_.Name })
  if ($off.Count -eq 0) { Out-ZcCheck '__FW__' 'PASS' 'Windows Firewall on (Domain, Private, Public)' }
  else {
    $other = @(Get-CimInstance -Namespace root\SecurityCenter2 -ClassName FirewallProduct -ErrorAction SilentlyContinue | Where-Object { $_.productState -band 0x1000 })
    if ($other.Count -gt 0) { Out-ZcCheck '__FW__' 'PASS' ('firewall on: ' + (($other | ForEach-Object { $_.displayName }) -join ', ')) }
    else { Out-ZcCheck '__FW__' 'FAIL' ('Windows Firewall off: ' + ($off -join ', ')) }
  }
} catch { Out-ZcCheck '__FW__' 'ERROR' "$($_.Exception.Message)" }

try {
  $av = @(Get-CimInstance -Namespace root\SecurityCenter2 -ClassName AntiVirusProduct -ErrorAction SilentlyContinue)
  if ($av.Count -gt 0) {
    $on = @($av | Where-Object { $_.productState -band 0x1000 })
    $current = @($on | Where-Object { -not ($_.productState -band 0x10) })
    if ($current.Count -gt 0) { Out-ZcCheck '__AV__' 'PASS' ((($current | ForEach-Object { $_.displayName }) -join ', ') + ' on, definitions up to date') }
    elseif ($on.Count -gt 0) { Out-ZcCheck '__AV__' 'FAIL' ((($on | ForEach-Object { $_.displayName }) -join ', ') + ' on, definitions out of date') }
    else { Out-ZcCheck '__AV__' 'FAIL' ('antivirus off: ' + (($av | ForEach-Object { $_.displayName }) -join ', ')) }
  } else {
    $m = Get-MpComputerStatus
    if ($m.AntivirusEnabled -and $m.RealTimeProtectionEnabled) { Out-ZcCheck '__AV__' 'PASS' ('Microsoft Defender on, definitions {0} day(s) old' -f $m.AntivirusSignatureAge) }
    else { Out-ZcCheck '__AV__' 'FAIL' 'Microsoft Defender real-time protection off' }
  }
} catch { Out-ZcCheck '__AV__' 'FAIL' 'no antivirus registered' }
"#;

/// True for a string shaped like a SID (`S-1-5-21-...`): the only text the
/// script embeds besides constants.
fn is_sid_shaped(s: &str) -> bool {
    let mut parts = s.split('-');
    parts.next() == Some("S")
        && s.split('-').count() >= 3
        && parts.all(|p| !p.is_empty() && p.bytes().all(|b| b.is_ascii_digit()))
}

/// The posture script for `user_sid` (the enrolling user).
pub(crate) fn script(user_sid: Option<&str>) -> String {
    let sid = user_sid.filter(|s| is_sid_shaped(s)).unwrap_or("");
    SCRIPT
        .replace("__ENC__", CHECK_ID_WINDOWS_ENCRYPTION)
        .replace("__LOCK__", CHECK_ID_WINDOWS_SCREEN_LOCK)
        .replace("__BIO__", CHECK_ID_WINDOWS_BIOMETRIC)
        .replace("__FW__", CHECK_ID_WINDOWS_FIREWALL)
        .replace("__AV__", CHECK_ID_WINDOWS_ANTIVIRUS)
        .replace("__PIN__", PIN_PROVIDER_KEY)
        .replace("__SID__", sid)
}

/// `powershell -EncodedCommand` takes base64 of UTF-16LE: no quoting of the
/// script on the command line.
pub(crate) fn encode_command(script: &str) -> String {
    let bytes: Vec<u8> = script
        .encode_utf16()
        .flat_map(|u| u.to_le_bytes())
        .collect();
    base64::engine::general_purpose::STANDARD.encode(bytes)
}

/// The `ZC|id|STATUS|detail` lines of the script's output; anything else
/// (warnings, progress) is ignored.
pub(crate) fn parse_output(stdout: &str) -> Vec<(String, String, String)> {
    stdout
        .lines()
        .filter_map(|line| {
            let mut f = line.trim_end_matches('\r').splitn(4, '|');
            if f.next()? != "ZC" {
                return None;
            }
            let id = f.next()?.to_string();
            let status = f.next()?.to_string();
            let detail = f.next().unwrap_or("").trim().to_string();
            Some((id, status, detail))
        })
        .collect()
}

/// The report's Windows checks: the script's results, plus an ERROR for any
/// check it didn't report (so a broken script is visible, never a PASS).
pub(crate) fn checks_from_output(stdout: Option<&str>) -> Vec<PostureCheck> {
    let mut checks = checks_from_platform(
        &WINDOWS_CHECK_IDS,
        stdout.map(parse_output).unwrap_or_default(),
    );
    for id in WINDOWS_CHECK_IDS {
        if !checks.iter().any(|c| c.check_id == id) {
            checks.push(error_check(id, "posture script did not report this check"));
        }
    }
    checks
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::grpc::client_v1::CheckStatus;

    #[test]
    fn the_script_embeds_only_a_sid_shaped_user() {
        let s = script(Some("S-1-5-21-3839229199-1140266588-3879859482-1000"));
        assert!(s.contains("$sid = 'S-1-5-21-3839229199-1140266588-3879859482-1000'"));
        for bad in [
            "S-1-5-21'; Remove-Item C:\\ -Recurse; '",
            "luffy",
            "S-1",
            "S-1-x-2",
            "",
        ] {
            assert!(script(Some(bad)).contains("$sid = ''"), "{bad}");
        }
        assert!(script(None).contains("$sid = ''"));
    }

    #[test]
    fn the_script_reports_every_check_and_has_no_placeholders_left() {
        let s = script(None);
        for id in WINDOWS_CHECK_IDS {
            assert!(s.contains(&format!("Out-ZcCheck '{id}'")), "{id}");
        }
        assert!(!s.contains("__"), "unreplaced placeholder");
        // `R` is PowerShell's alias for Invoke-History and would shadow a
        // function of that name.
        assert!(!s.contains("\nfunction R("));
    }

    #[test]
    fn the_command_is_base64_utf16le() {
        assert_eq!(encode_command("ab"), "YQBiAA==");
    }

    #[test]
    fn output_lines_are_parsed_and_noise_ignored() {
        let out = "WARNING: something\r\nZC|windows.firewall.enabled|PASS|Windows Firewall on\r\n\
                   ZC|windows.antivirus.enabled|FAIL|antivirus off: X|Y\r\nnot ours\r\n";
        assert_eq!(
            parse_output(out),
            vec![
                (
                    "windows.firewall.enabled".into(),
                    "PASS".into(),
                    "Windows Firewall on".into()
                ),
                (
                    "windows.antivirus.enabled".into(),
                    "FAIL".into(),
                    "antivirus off: X|Y".into()
                ),
            ]
        );
    }

    #[test]
    fn missing_checks_are_errors_never_passes() {
        let checks = checks_from_output(Some("ZC|windows.firewall.enabled|PASS|on\n"));
        assert_eq!(checks.len(), 5);
        for c in &checks {
            let want = if c.check_id == CHECK_ID_WINDOWS_FIREWALL {
                CheckStatus::Pass
            } else {
                CheckStatus::Error
            };
            assert_eq!(c.status, want as i32, "{}", c.check_id);
        }
        let none = checks_from_output(None);
        assert!(none.iter().all(|c| c.status == CheckStatus::Error as i32));
    }

    #[test]
    fn foreign_or_lower_case_results_never_pass() {
        let checks = checks_from_output(Some(
            "ZC|linux.firewall.active|PASS|x\nZC|windows.screen_lock.enabled|pass|x\n",
        ));
        assert!(!checks.iter().any(|c| c.check_id == "linux.firewall.active"));
        let lock = checks
            .iter()
            .find(|c| c.check_id == CHECK_ID_WINDOWS_SCREEN_LOCK)
            .unwrap();
        assert_eq!(lock.status, CheckStatus::Error as i32);
    }
}
