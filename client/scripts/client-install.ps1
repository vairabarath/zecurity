<#
.SYNOPSIS
  ZECURITY client installer for Windows (counterpart of client-install.sh).

.DESCRIPTION
  Installs:
    - C:\Program Files\Zecurity\zecurity-client.exe   (the binary)
    - C:\Program Files\Zecurity\wintun.dll            (Wintun TUN driver DLL)
    - C:\ProgramData\Zecurity\client.conf             (optional controller addresses)
    - the "zecurity-client" Windows service           (LocalSystem, automatic start)

  The service runs as LocalSystem because creating the Wintun adapter and
  routes requires it. Its command line carries the enrolling user's SID
  (--ipc-user-sid): the one non-admin account allowed to use the daemon's
  IPC pipe and its data directory. The service configuration is admin-only,
  so that user can't change who else is admitted.

  Run from an elevated PowerShell. The enrolling user is the user logged on
  at the console by default (correct for UAC over-the-shoulder elevation);
  pass -User to choose another.

  The enrolling user can read the data directory (and clear stale state)
  but not change it: the SYSTEM daemon must never act on a folder a user
  could swap for a junction. So the workspace is set here (-Workspace), or
  later with `zecurity-client setup` from an elevated prompt.

  After install, as the enrolling user (normal, non-elevated prompt):
    zecurity-client login
    zecurity-client up

.PARAMETER Binary
  Path to zecurity-client.exe (a build that embeds the controller's platform
  root CA, see client/trust/README.md).

.PARAMETER Wintun
  Path to wintun.dll for this architecture (amd64), from the official
  Wintun release https://www.wintun.net (wintun\bin\amd64\wintun.dll).

.PARAMETER User
  Enrolling user (DOMAIN\name or name). Default: the console user.

.PARAMETER Workspace
  Workspace name (what `zecurity-client setup --workspace` sets).

.PARAMETER ControllerAddr
  Pre-configure the controller gRPC address, e.g. 192.168.1.10:9090.

.PARAMETER ControllerHttpAddr
  Pre-configure the controller HTTP base, e.g. 192.168.1.10:8080.

.PARAMETER Force
  Stop and remove an existing install first.

.PARAMETER Uninstall
  Stop and remove the service, binary and PATH entry. Keeps
  C:\ProgramData\Zecurity (device state); delete it by hand to forget the
  device.

.EXAMPLE
  .\client-install.ps1 -Binary .\zecurity-client.exe -Wintun .\wintun.dll -ControllerAddr 192.168.1.10:9090 -ControllerHttpAddr 192.168.1.10:8080
#>
[CmdletBinding(DefaultParameterSetName = 'Install')]
param(
    [Parameter(ParameterSetName = 'Install', Mandatory = $true)][string]$Binary,
    [Parameter(ParameterSetName = 'Install', Mandatory = $true)][string]$Wintun,
    [Parameter(ParameterSetName = 'Install')][string]$User,
    [Parameter(ParameterSetName = 'Install')][string]$Workspace,
    [Parameter(ParameterSetName = 'Install')][string]$ControllerAddr,
    [Parameter(ParameterSetName = 'Install')][string]$ControllerHttpAddr,
    [Parameter(ParameterSetName = 'Install')][switch]$Force,
    [Parameter(ParameterSetName = 'Uninstall', Mandatory = $true)][switch]$Uninstall
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$ServiceName = 'zecurity-client'
$InstallDir  = Join-Path $env:ProgramFiles 'Zecurity'
$InstallBin  = Join-Path $InstallDir 'zecurity-client.exe'
$DataDir     = Join-Path $env:ProgramData 'Zecurity'
$ConfigFile  = Join-Path $DataDir 'client.conf'

function Log([string]$m)  { Write-Host "[install] $m" -ForegroundColor Cyan }
function Fail([string]$m) { Write-Host "[install] $m" -ForegroundColor Red; exit 1 }

# ── Checks ──────────────────────────────────────────────────────────────────
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Fail 'must run from an elevated PowerShell (Run as administrator)'
}
if ($env:PROCESSOR_ARCHITECTURE -ne 'AMD64') {
    Fail "unsupported architecture: $($env:PROCESSOR_ARCHITECTURE) (amd64 only for now)"
}

function Remove-Install {
    $svc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if ($svc) {
        Log "stopping and removing service $ServiceName"
        if ($svc.Status -ne 'Stopped') { Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue }
        & sc.exe delete $ServiceName | Out-Null
        Start-Sleep -Seconds 1
    }
    if (Test-Path $InstallDir) {
        Log "removing $InstallDir"
        Remove-Item -Recurse -Force $InstallDir
    }
    $path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    $parts = $path -split ';' | Where-Object { $_ -and ($_.TrimEnd('\') -ne $InstallDir) }
    [Environment]::SetEnvironmentVariable('Path', ($parts -join ';'), 'Machine')
}

if ($Uninstall) {
    Remove-Install
    Log "uninstalled; device state kept in $DataDir"
    exit 0
}

if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    if (-not $Force) { Fail "already installed; re-run with -Force to reinstall" }
    Remove-Install
}

foreach ($f in @($Binary, $Wintun)) {
    if (-not (Test-Path -PathType Leaf $f)) { Fail "file not found: $f" }
}

# ── Enrolling user → SID ────────────────────────────────────────────────────
if (-not $User) {
    $User = (Get-CimInstance Win32_ComputerSystem).UserName
    if (-not $User) { $User = [Security.Principal.WindowsIdentity]::GetCurrent().Name }
}
try {
    $Sid = (New-Object Security.Principal.NTAccount($User)).Translate([Security.Principal.SecurityIdentifier]).Value
} catch {
    Fail "could not resolve user '$User' to a SID: $_"
}
if ($Sid -notmatch '^S-1-\d+(-\d+)+$') { Fail "unexpected SID format: $Sid" }
if ($Sid -eq 'S-1-5-18') { Fail 'the enrolling user must be a person, not SYSTEM (pass -User)' }
Log "installing for user: $User ($Sid)"

# ── Binary + wintun.dll (admin-only directory) ──────────────────────────────
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Copy-Item -Force $Binary $InstallBin
Copy-Item -Force $Wintun (Join-Path $InstallDir 'wintun.dll')
Log "installed binary to $InstallBin"

# ── Data directory (config, encrypted state, logs) ──────────────────────────
# C:\ProgramData lets any user create folders: a folder someone else made
# first is refused, never adopted. The daemon re-checks and re-applies this
# on every start (win_security.rs).
$trusted = @('S-1-5-18', 'S-1-5-32-544')
if (Test-Path $DataDir) {
    $ownerSid = (Get-Acl $DataDir).GetOwner([Security.Principal.SecurityIdentifier]).Value
    if ($trusted -notcontains $ownerSid) {
        Fail "$DataDir already exists and is owned by $ownerSid, not SYSTEM or Administrators. Inspect and remove it, then re-run."
    }
} else {
    New-Item -ItemType Directory -Path $DataDir | Out-Null
}
& icacls.exe $DataDir /setowner '*S-1-5-32-544' | Out-Null
# SYSTEM and Administrators: full control. The user: list the folder and
# read its files, nothing else. (The daemon sets the same on every start, and
# the state and logs folders' own rules; see win_security.rs.)
& icacls.exe $DataDir /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' "*${Sid}:(RX)" "*${Sid}:(OI)(IO)(R)" | Out-Null
if ($LASTEXITCODE -ne 0) { Fail "could not restrict access to $DataDir" }
Log "data directory: $DataDir (SYSTEM and Administrators; $User may read)"

if ($Workspace -and $Workspace -notmatch '^[A-Za-z0-9._-]+$') { Fail "invalid workspace name: $Workspace" }
if ($Workspace -or $ControllerAddr -or $ControllerHttpAddr) {
    $lines = @()
    if (Test-Path $ConfigFile) {
        $lines = Get-Content $ConfigFile | Where-Object { $_ -notmatch '^\s*(workspace|controller_address|http_base_url)\s*=' }
        $old = Get-Content $ConfigFile | Where-Object { $_ -match '^\s*workspace\s*=' }
        if (-not $Workspace -and $old) { $lines = @($old) + $lines }
    }
    if ($Workspace) { $lines = @("workspace = `"$Workspace`"") + $lines }
    elseif (-not ($lines | Where-Object { $_ -match '^\s*workspace\s*=' })) { $lines = @('workspace = ""') + $lines }
    if ($ControllerAddr)     { $lines += "controller_address = `"$ControllerAddr`"" }
    if ($ControllerHttpAddr) {
        $base = if ($ControllerHttpAddr -match '^https?://') { $ControllerHttpAddr } else { "http://$ControllerHttpAddr" }
        $lines += "http_base_url = `"$base`""
    }
    Set-Content -Path $ConfigFile -Value $lines -Encoding ascii
    Log "wrote $ConfigFile"
}

# ── Service ─────────────────────────────────────────────────────────────────
$binPath = "`"$InstallBin`" daemon --ipc-user-sid $Sid"
& sc.exe create $ServiceName binPath= $binPath start= auto obj= LocalSystem DisplayName= 'Zecurity Client' | Out-Null
if ($LASTEXITCODE -ne 0) { Fail "sc.exe create failed ($LASTEXITCODE)" }
& sc.exe description $ServiceName 'Zecurity ZTNA client daemon' | Out-Null
# Restart on failure, like Restart=on-failure: after a crash, and (failureflag)
# after a non-zero exit, e.g. no workspace configured yet. A deliberate stop
# (device revoked, shutdown request) exits 0 and is not restarted.
& sc.exe failure $ServiceName reset= 86400 actions= restart/5000/restart/5000/restart/30000 | Out-Null
& sc.exe failureflag $ServiceName 1 | Out-Null

# Let the enrolling user start, stop and query the service (the CLI starts it
# when the daemon isn't answering); everything else stays admin-only.
$sd = ((& sc.exe sdshow $ServiceName) | Where-Object { $_.Trim() }) -join ''
$ace = "(A;;RPWPLCRC;;;$Sid)"
if ($sd -notlike "*$ace*") {
    $newSd = if ($sd.Contains('S:')) { $sd.Replace('S:', "${ace}S:") } else { "$sd$ace" }
    & sc.exe sdset $ServiceName $newSd | Out-Null
    if ($LASTEXITCODE -ne 0) { Fail "sc.exe sdset failed ($LASTEXITCODE)" }
}

# ── PATH ────────────────────────────────────────────────────────────────────
$path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
if (($path -split ';' | ForEach-Object { $_.TrimEnd('\') }) -notcontains $InstallDir) {
    [Environment]::SetEnvironmentVariable('Path', "$path;$InstallDir", 'Machine')
    Log "added $InstallDir to the system PATH (open a new terminal to use it)"
}

# Like `systemctl enable --now`: until a workspace is set the daemon exits and
# the service retries; it picks up the config on its next start.
Start-Service -Name $ServiceName -ErrorAction SilentlyContinue
Log "service $ServiceName installed (logs: $DataDir\logs\daemon.log)"
Log 'next, as the enrolling user in a normal (non-elevated) terminal:'
if (-not $Workspace) {
    Log '  (elevated) zecurity-client setup --workspace <workspace-name>'
    Log '  sc.exe stop zecurity-client; sc.exe start zecurity-client   # reload the config'
}
Log '  zecurity-client login'
Log '  zecurity-client up'
