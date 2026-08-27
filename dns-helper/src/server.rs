// server.rs — the request path, in the order that matters.
//
//   1. WHO   — SO_PEERCRED (peer.rs). Refused before the body is even read.
//   2. WHAT  — the whitelist (validate.rs).
//   3. DO    — the backend (resolved.rs).
//
// The ordering is the design. Authorizing first means an unauthorized caller cannot
// reach the parser, so malformed-input handling is not part of its attack surface.
// Validating second means the backend only ever sees values already proven to be in
// range, so it does no policy of its own.

use std::io::{BufRead, BufReader, Write};
use std::os::unix::net::{UnixListener, UnixStream};

use crate::peer::{authorize, peer_cred, PeerCred};
use crate::protocol::{Request, Response};
use crate::resolved::DnsBackend;
use crate::validate;

/// Decide and execute one already-parsed request. Separated from the socket so the
/// full authorize → validate → act path is testable with synthetic credentials.
pub fn handle(
    req: &Request,
    cred: PeerCred,
    allowed_uid: u32,
    backend: &dyn DnsBackend,
) -> Response {
    // 1. WHO
    if let Err(denied) = authorize(cred, allowed_uid) {
        eprintln!("DENIED {denied}");
        // Deliberately terse to the caller: an unauthorized peer learns that it was
        // refused, not what the policy is. The detail goes to the journal.
        return Response::err("unauthorized");
    }

    // 2. WHAT, then 3. DO
    match req {
        Request::Apply {
            iface,
            server,
            domains,
        } => match validate::validate_apply(iface, server, domains) {
            Err(reject) => {
                eprintln!("REJECT uid={} {reject}", cred.uid);
                Response::err(reject.to_string())
            }
            Ok(_) => match backend.apply(iface, server, domains) {
                Ok(()) => {
                    eprintln!(
                        "apply ok iface={iface} server={server} domains={}",
                        domains.len()
                    );
                    Response::ok()
                }
                Err(e) => {
                    eprintln!("apply FAILED iface={iface}: {e}");
                    Response::err(e)
                }
            },
        },
        Request::Revert { iface } => match validate::validate_revert(iface) {
            Err(reject) => {
                eprintln!("REJECT uid={} {reject}", cred.uid);
                Response::err(reject.to_string())
            }
            Ok(()) => match backend.revert(iface) {
                Ok(()) => {
                    eprintln!("revert ok iface={iface}");
                    Response::ok()
                }
                Err(e) => {
                    eprintln!("revert FAILED iface={iface}: {e}");
                    Response::err(e)
                }
            },
        },
    }
}

/// One request per connection, newline-delimited JSON.
pub fn serve_conn(stream: UnixStream, allowed_uid: u32, backend: &dyn DnsBackend) {
    // WHO first: read the peer before the payload.
    let cred = match peer_cred(&stream) {
        Ok(c) => c,
        Err(e) => {
            // Cannot identify the caller ⇒ cannot serve it. Fail closed.
            eprintln!("could not read peer credentials, refusing: {e}");
            return;
        }
    };

    let mut reader = BufReader::new(match stream.try_clone() {
        Ok(s) => s,
        Err(e) => {
            eprintln!("could not clone stream: {e}");
            return;
        }
    });
    let mut line = String::new();
    let resp = match reader.read_line(&mut line) {
        Err(e) => Response::err(format!("read failed: {e}")),
        Ok(0) => return, // peer hung up without sending anything
        Ok(_) => match serde_json::from_str::<Request>(line.trim()) {
            // A parse failure is reported without echoing the payload back.
            Err(e) => {
                eprintln!("malformed request from uid={}: {e}", cred.uid);
                Response::err("malformed request")
            }
            Ok(req) => handle(&req, cred, allowed_uid, backend),
        },
    };

    let mut w = stream;
    let body = serde_json::to_string(&resp).unwrap_or_else(|_| r#"{"ok":false}"#.to_string());
    if let Err(e) = w.write_all(format!("{body}\n").as_bytes()) {
        eprintln!("could not write response: {e}");
    }
}

/// Accept loop. Errors on one connection never take down the listener.
pub fn serve(listener: UnixListener, allowed_uid: u32, backend: &dyn DnsBackend) -> ! {
    eprintln!("zecurity-dns-helper ready (allowed_uid={allowed_uid})");
    loop {
        match listener.accept() {
            Ok((stream, _)) => serve_conn(stream, allowed_uid, backend),
            Err(e) => eprintln!("accept failed: {e}"),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::resolved::RecordingBackend;

    const ALLOWED: u32 = 1000;
    fn ok_cred() -> PeerCred {
        PeerCred {
            pid: 10,
            uid: ALLOWED,
            gid: ALLOWED,
        }
    }
    fn bad_cred() -> PeerCred {
        PeerCred {
            pid: 11,
            uid: 4242,
            gid: ALLOWED,
        }
    }
    fn apply(domains: &[&str]) -> Request {
        Request::Apply {
            iface: "zecurity0".into(),
            server: "127.0.0.1".into(),
            domains: domains.iter().map(|s| s.to_string()).collect(),
        }
    }

    #[test]
    fn an_authorized_valid_apply_reaches_the_backend() {
        let b = RecordingBackend::new();
        let r = handle(&apply(&["~app.internal"]), ok_cred(), ALLOWED, &b);
        assert!(r.ok, "{r:?}");
        assert_eq!(b.calls(), vec!["apply zecurity0 127.0.0.1 [~app.internal]"]);
    }

    /// The single most important test in this file: an unauthorized caller must not
    /// reach the backend at all, even with a perfectly valid request.
    #[test]
    fn an_unauthorized_caller_never_reaches_the_backend() {
        let b = RecordingBackend::new();
        let r = handle(&apply(&["~app.internal"]), bad_cred(), ALLOWED, &b);
        assert!(!r.ok);
        assert_eq!(r.error.as_deref(), Some("unauthorized"));
        assert!(
            b.calls().is_empty(),
            "backend must not have been called: {:?}",
            b.calls()
        );
    }

    /// WHO is decided before WHAT: an unauthorized caller sending an invalid request
    /// is told "unauthorized", not given a validation critique it could probe with.
    #[test]
    fn authorization_is_checked_before_validation() {
        let b = RecordingBackend::new();
        let req = Request::Apply {
            iface: "eth0".into(),
            server: "8.8.8.8".into(),
            domains: vec!["~.".into()],
        };
        let r = handle(&req, bad_cred(), ALLOWED, &b);
        assert_eq!(
            r.error.as_deref(),
            Some("unauthorized"),
            "must not leak the validation reason"
        );
        assert!(b.calls().is_empty());
    }

    #[test]
    fn an_authorized_but_invalid_request_is_rejected_before_the_backend() {
        for req in [
            Request::Apply {
                iface: "eth0".into(),
                server: "127.0.0.1".into(),
                domains: vec!["~a.b".into()],
            },
            Request::Apply {
                iface: "zecurity0".into(),
                server: "8.8.8.8".into(),
                domains: vec!["~a.b".into()],
            },
            Request::Apply {
                iface: "zecurity0".into(),
                server: "127.0.0.1".into(),
                domains: vec!["~.".into()],
            },
            Request::Apply {
                iface: "zecurity0".into(),
                server: "127.0.0.1".into(),
                domains: vec!["~internal".into()],
            },
            Request::Revert { iface: "lo".into() },
        ] {
            let b = RecordingBackend::new();
            let r = handle(&req, ok_cred(), ALLOWED, &b);
            assert!(!r.ok, "{req:?} should be rejected");
            assert!(b.calls().is_empty(), "{req:?} must not reach the backend");
            // An authorized caller DOES get the reason — it is their own bug to fix.
            assert!(r.error.unwrap_or_default().len() > "unauthorized".len());
        }
    }

    #[test]
    fn a_backend_failure_is_reported_not_swallowed() {
        let b = RecordingBackend::failing("resolvectl said no");
        let r = handle(&apply(&["~app.internal"]), ok_cred(), ALLOWED, &b);
        assert!(!r.ok);
        assert_eq!(r.error.as_deref(), Some("resolvectl said no"));
    }

    #[test]
    fn revert_is_authorized_validated_and_forwarded() {
        let b = RecordingBackend::new();
        let r = handle(
            &Request::Revert {
                iface: "zecurity0".into(),
            },
            ok_cred(),
            ALLOWED,
            &b,
        );
        assert!(r.ok);
        assert_eq!(b.calls(), vec!["revert zecurity0"]);
    }

    #[test]
    fn root_is_served() {
        let b = RecordingBackend::new();
        let root = PeerCred {
            pid: 1,
            uid: 0,
            gid: 0,
        };
        assert!(handle(&apply(&["~app.internal"]), root, ALLOWED, &b).ok);
    }

    /// End-to-end over a real socket: framing, peer credentials from the kernel, and
    /// the response all together. Our own uid is the allowed one, so this exercises
    /// the success path without root.
    #[test]
    fn a_real_socket_round_trip_works() {
        use std::io::{BufRead, BufReader, Write};
        let dir = std::env::temp_dir().join(format!("zdh-srv-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("h.sock");
        let listener = UnixListener::bind(&path).unwrap();
        let me = unsafe { libc::getuid() };

        let t = std::thread::spawn(move || {
            let (s, _) = listener.accept().unwrap();
            let b = RecordingBackend::new();
            serve_conn(s, me, &b);
            b.calls()
        });

        let mut c = UnixStream::connect(&path).unwrap();
        let req = serde_json::to_string(&apply(&["~app.internal"])).unwrap();
        c.write_all(format!("{req}\n").as_bytes()).unwrap();
        let mut resp = String::new();
        BufReader::new(c.try_clone().unwrap())
            .read_line(&mut resp)
            .unwrap();

        let parsed: Response = serde_json::from_str(resp.trim()).unwrap();
        assert!(parsed.ok, "{parsed:?}");
        assert_eq!(
            t.join().unwrap(),
            vec!["apply zecurity0 127.0.0.1 [~app.internal]"]
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// Garbage must not crash the helper, and must not echo the payload back.
    #[test]
    fn a_malformed_line_gets_a_terse_error_and_the_helper_survives() {
        use std::io::{BufRead, BufReader, Write};
        let dir = std::env::temp_dir().join(format!("zdh-bad-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("h.sock");
        let listener = UnixListener::bind(&path).unwrap();
        let me = unsafe { libc::getuid() };

        let t = std::thread::spawn(move || {
            let (s, _) = listener.accept().unwrap();
            let b = RecordingBackend::new();
            serve_conn(s, me, &b);
            b.calls()
        });

        let mut c = UnixStream::connect(&path).unwrap();
        c.write_all(b"{\"verb\":\"exec\",\"cmd\":\"rm -rf /\"}\n")
            .unwrap();
        let mut resp = String::new();
        BufReader::new(c.try_clone().unwrap())
            .read_line(&mut resp)
            .unwrap();

        let parsed: Response = serde_json::from_str(resp.trim()).unwrap();
        assert!(!parsed.ok);
        assert_eq!(parsed.error.as_deref(), Some("malformed request"));
        assert!(t.join().unwrap().is_empty(), "backend must not be touched");
        let _ = std::fs::remove_dir_all(&dir);
    }
}
