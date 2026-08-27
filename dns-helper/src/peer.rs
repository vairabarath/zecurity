// peer.rs — "who may ask", the other half of the authorization model.
//
// `validate` decides WHAT is permitted; this decides WHO. Neither is sufficient
// alone: a perfect whitelist still lets any local user reconfigure our link, and a
// perfect peer check still lets the daemon ask for anything.
//
// ⚠️ Socket file permissions are NOT the check. A `root:zecurity 0660` socket
// authorizes *every* member of that group, and group membership is administered
// elsewhere and changes without us knowing. `SO_PEERCRED` asks the kernel who is
// actually on the other end. The socket mode remains as defence in depth.
//
// `libc` is used because std's `UnixStream::peer_cred()` is still unstable
// (`peer_credentials_unix_socket`, rust-lang/rust#42839).

use std::io;
use std::os::unix::io::AsRawFd;
use std::os::unix::net::UnixStream;

/// Kernel-reported identity of the process at the other end of a Unix socket.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct PeerCred {
    pub pid: i32,
    pub uid: u32,
    pub gid: u32,
}

/// Why a caller was refused. Kept distinct from `validate::Reject` because the two
/// answer different questions and an operator debugging one should not be shown the
/// other's vocabulary.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Denied {
    pub cred: PeerCred,
    pub allowed_uid: u32,
}

impl std::fmt::Display for Denied {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "refusing request from uid {} (pid {}): only uid {} or root may call this helper",
            self.cred.uid, self.cred.pid, self.allowed_uid
        )
    }
}

/// Ask the kernel who is on the other end of `stream`.
pub fn peer_cred(stream: &UnixStream) -> io::Result<PeerCred> {
    // SAFETY: `ucred` is a plain repr(C) struct of three integers; we hand
    // getsockopt a correctly-sized, zeroed buffer and its own length, and check the
    // return code before reading it back.
    let mut cred: libc::ucred = unsafe { std::mem::zeroed() };
    let mut len = std::mem::size_of::<libc::ucred>() as libc::socklen_t;
    let rc = unsafe {
        libc::getsockopt(
            stream.as_raw_fd(),
            libc::SOL_SOCKET,
            libc::SO_PEERCRED,
            &mut cred as *mut libc::ucred as *mut libc::c_void,
            &mut len,
        )
    };
    if rc != 0 {
        return Err(io::Error::last_os_error());
    }
    Ok(PeerCred {
        pid: cred.pid,
        uid: cred.uid,
        gid: cred.gid,
    })
}

/// May this caller be served?
///
/// Root is allowed alongside the configured uid, deliberately: root can call
/// `resolvectl` directly and bypass this helper entirely, so refusing it would buy no
/// security while breaking administrative testing. Every other uid is refused
/// regardless of group membership.
pub fn authorize(cred: PeerCred, allowed_uid: u32) -> Result<(), Denied> {
    if cred.uid == allowed_uid || cred.uid == 0 {
        Ok(())
    } else {
        Err(Denied { cred, allowed_uid })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::net::UnixListener;

    /// Round-trips a real socket rather than a mock: the point of this module is that
    /// the *kernel* reports the peer, so a fake would test nothing.
    #[test]
    fn the_kernel_reports_our_own_uid_over_a_real_socket() {
        let dir = std::env::temp_dir().join(format!("zdh-peer-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("s.sock");

        let listener = UnixListener::bind(&path).unwrap();
        let client = UnixStream::connect(&path).unwrap();
        let (server, _) = listener.accept().unwrap();

        let me = unsafe { libc::getuid() };
        let from_server = peer_cred(&server).unwrap();
        let from_client = peer_cred(&client).unwrap();
        assert_eq!(from_server.uid, me, "server side sees the client's uid");
        assert_eq!(from_client.uid, me, "client side sees the server's uid");
        assert!(from_server.pid > 0);

        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn the_configured_uid_is_allowed() {
        let c = PeerCred {
            pid: 42,
            uid: 1000,
            gid: 1000,
        };
        assert!(authorize(c, 1000).is_ok());
    }

    /// Root can bypass the helper anyway, so refusing it would cost usability for no
    /// security.
    #[test]
    fn root_is_allowed() {
        let c = PeerCred {
            pid: 1,
            uid: 0,
            gid: 0,
        };
        assert!(authorize(c, 1000).is_ok());
    }

    /// The rejection that matters: another local user, even one in the socket's group.
    #[test]
    fn any_other_uid_is_refused() {
        for uid in [1, 999, 1001, 65534] {
            let c = PeerCred {
                pid: 7,
                uid,
                gid: 1000,
            };
            assert_eq!(
                authorize(c, 1000),
                Err(Denied {
                    cred: c,
                    allowed_uid: 1000
                }),
                "uid {uid} must be refused"
            );
        }
    }

    /// Group membership must not be a route in — that is exactly what socket mode
    /// alone would have granted.
    #[test]
    fn a_matching_gid_does_not_authorize() {
        let c = PeerCred {
            pid: 7,
            uid: 1234,
            gid: 1000,
        };
        assert!(authorize(c, 1000).is_err(), "gid must be irrelevant");
    }

    #[test]
    fn a_denial_names_the_caller_and_the_expected_uid() {
        let c = PeerCred {
            pid: 4321,
            uid: 31337,
            gid: 5,
        };
        let msg = authorize(c, 1000).unwrap_err().to_string();
        assert!(
            msg.contains("31337") && msg.contains("4321") && msg.contains("1000"),
            "got: {msg}"
        );
    }
}
