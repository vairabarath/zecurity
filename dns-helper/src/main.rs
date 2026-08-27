// zecurity-dns-helper — ADR-023 option C.
//
// Runs as root so it can configure per-link DNS via systemd-resolved, which polkit
// gates behind `auth_admin` (see ADR-023). Everything that decides whether to act
// lives in the library: `peer` answers WHO, `validate` answers WHAT. This binary is a
// shell that wires a socket to those two and, only on their approval, to `resolved`.
//
// Normally started by systemd socket activation, so root runs only while a DNS change
// is being applied. `--listen` exists for testing without systemd.

use std::os::unix::io::FromRawFd;
use std::os::unix::net::UnixListener;
use std::process::ExitCode;

use zecurity_dns_helper::resolved::ResolvedBackend;
use zecurity_dns_helper::server;

/// systemd hands the first listening socket in at fd 3 (`SD_LISTEN_FDS_START`).
const SD_LISTEN_FDS_START: i32 = 3;

const USAGE: &str = "\
zecurity-dns-helper — privileged per-link DNS helper for the Zecurity TUN

USAGE:
    zecurity-dns-helper --allow-uid <UID> [--listen <PATH>]

OPTIONS:
    --allow-uid <UID>   The ONLY uid permitted to call this helper, besides root.
                        Required — there is no default, because guessing it wrong
                        would either lock out the daemon or widen access.
    --listen <PATH>     Bind this path instead of using socket activation.
                        For testing only; production uses the .socket unit.

The helper refuses anything outside its whitelist: the interface must be the Zecurity
TUN, the server must be loopback or inside 100.64.0.0/10, and every domain must be an
individual routing-only FQDN. See ADR-023.
";

fn main() -> ExitCode {
    let mut allow_uid: Option<u32> = None;
    let mut listen: Option<String> = None;

    let mut args = std::env::args().skip(1);
    while let Some(a) = args.next() {
        match a.as_str() {
            "--allow-uid" => match args.next().and_then(|v| v.parse::<u32>().ok()) {
                Some(u) => allow_uid = Some(u),
                None => {
                    eprintln!("--allow-uid requires a numeric uid");
                    return ExitCode::from(2);
                }
            },
            "--listen" => match args.next() {
                Some(p) => listen = Some(p),
                None => {
                    eprintln!("--listen requires a path");
                    return ExitCode::from(2);
                }
            },
            "-h" | "--help" => {
                print!("{USAGE}");
                return ExitCode::SUCCESS;
            }
            other => {
                eprintln!("unknown argument {other:?}\n\n{USAGE}");
                return ExitCode::from(2);
            }
        }
    }

    let Some(allow_uid) = allow_uid else {
        eprintln!("--allow-uid is required\n\n{USAGE}");
        return ExitCode::from(2);
    };

    // Refuse to run unprivileged rather than failing later, mid-request, with a
    // confusing resolvectl permission error.
    let uid = unsafe { libc::getuid() };
    if uid != 0 {
        eprintln!(
            "must run as root (running as uid {uid}): configuring per-link DNS is \
             polkit-gated behind auth_admin. See ADR-023."
        );
        return ExitCode::from(1);
    }

    let listener = match socket_activated() {
        Some(l) => {
            eprintln!("using socket activation (fd {SD_LISTEN_FDS_START})");
            l
        }
        None => match listen {
            Some(path) => {
                // Stale socket from a previous run: same reconcile-then-apply pattern
                // the client uses for its own IPC socket and policy routes.
                let _ = std::fs::remove_file(&path);
                match UnixListener::bind(&path) {
                    Ok(l) => {
                        eprintln!("listening on {path}");
                        l
                    }
                    Err(e) => {
                        eprintln!("could not bind {path}: {e}");
                        return ExitCode::from(1);
                    }
                }
            }
            None => {
                eprintln!("no socket: expected systemd socket activation or --listen\n\n{USAGE}");
                return ExitCode::from(2);
            }
        },
    };

    server::serve(listener, allow_uid, &ResolvedBackend)
}

/// Take the listener systemd passed us, if it did.
///
/// `LISTEN_PID` is checked against our own pid deliberately: the variables are
/// inherited by children, so without it a subprocess could mistake a parent's socket
/// for its own.
fn socket_activated() -> Option<UnixListener> {
    if std::env::var("LISTEN_PID").ok()?.parse::<i32>().ok()? != unsafe { libc::getpid() } {
        return None;
    }
    if std::env::var("LISTEN_FDS").ok()?.parse::<i32>().ok()? != 1 {
        eprintln!("expected exactly one socket from systemd");
        return None;
    }
    // SAFETY: systemd guarantees fd 3 is an open, listening socket when LISTEN_FDS=1
    // and LISTEN_PID matches, and we take ownership of it exactly once.
    Some(unsafe { UnixListener::from_raw_fd(SD_LISTEN_FDS_START) })
}
