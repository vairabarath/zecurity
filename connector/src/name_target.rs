//! PENDING-14 F2: where a tunnel to a resource actually goes.
//!
//! A resource's destination is an IP literal or, from PENDING-14, a DNS name.
//! The connector authorizes on the destination string exactly as before (the
//! ACL entry's address), then resolves a name **inside the remote network**
//! with its own resolver, keeps only addresses a resource may have, and
//! connects to the address it checked. It never hands `"name:port"` to a
//! socket call, so the name can't be resolved again to something else between
//! the check and the connect.
//!
//! IP destinations are unchanged: they are used as given.

use std::net::{IpAddr, SocketAddr};

/// The error text a client gets when a name has no usable address.
pub const ERR_NAME_UNRESOLVED: &str = "NAME_UNRESOLVED";

/// The error text a client gets when a shield-protected name resolves to an
/// address that isn't the shield's (the shield refuses it, F-5).
pub const ERR_NAME_NOT_ON_SHIELD: &str = "NAME_NOT_ON_SHIELD";

#[derive(Debug, PartialEq, Eq)]
pub enum ResolveError {
    /// The lookup itself failed (NXDOMAIN, resolver down, timeout).
    Lookup(String),
    /// The name resolved, but only to addresses a resource may not have.
    NoAllowedAddress(Vec<IpAddr>),
}

impl std::fmt::Display for ResolveError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            ResolveError::Lookup(e) => write!(f, "name lookup failed: {e}"),
            ResolveError::NoAllowedAddress(addrs) => {
                write!(
                    f,
                    "name resolved only to addresses that are not allowed: {addrs:?}"
                )
            }
        }
    }
}

/// Whether `destination` is a DNS name (not an IP literal).
pub fn is_name(destination: &str) -> bool {
    destination.parse::<IpAddr>().is_err()
}

/// Whether a resolved address may be a resource's address (F-3). IPv4 only in
/// v1; never loopback (the connector itself), link-local (169.254/16, incl.
/// cloud metadata at 169.254.169.254), unspecified, multicast, broadcast, or
/// `198.18.0.0/15`, the range Zecurity clients use for DNS-name resources'
/// fake addresses (F-7). A client on the connector's own host answers the
/// names with fake addresses through systemd-resolved; dialling one would
/// loop the tunnel back into that client (found in the F4 live run).
pub fn allowed_resolved(ip: IpAddr) -> bool {
    match ip {
        IpAddr::V4(v4) => {
            let o = v4.octets();
            !(v4.is_loopback()
                || v4.is_link_local()
                || v4.is_unspecified()
                || v4.is_multicast()
                || v4.is_broadcast()
                || (o[0] == 198 && (o[1] == 18 || o[1] == 19)))
        }
        IpAddr::V6(_) => false,
    }
}

/// The first allowed address among a lookup's results, in resolver order.
pub fn pick_allowed(
    addrs: impl IntoIterator<Item = SocketAddr>,
) -> Result<SocketAddr, ResolveError> {
    let mut seen = Vec::new();
    for addr in addrs {
        if allowed_resolved(addr.ip()) {
            return Ok(addr);
        }
        seen.push(addr.ip());
    }
    Err(ResolveError::NoAllowedAddress(seen))
}

/// The socket address to dial for `destination:port`. An IP literal is used as
/// given. A name is resolved with the system resolver and filtered by
/// [`allowed_resolved`].
pub async fn resolve_target(destination: &str, port: u16) -> Result<SocketAddr, ResolveError> {
    if let Ok(ip) = destination.parse::<IpAddr>() {
        return Ok(SocketAddr::new(ip, port));
    }
    let addrs = tokio::net::lookup_host((destination, port))
        .await
        .map_err(|e| ResolveError::Lookup(e.to_string()))?;
    pick_allowed(addrs)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sa(s: &str) -> SocketAddr {
        s.parse().unwrap()
    }

    #[test]
    fn allowed_addresses() {
        for ok in [
            "10.0.0.5",
            "192.168.1.33",
            "172.16.0.1",
            "198.17.255.255",
            "198.20.0.1",
            "8.8.8.8",
            "100.64.1.1",
        ] {
            assert!(
                allowed_resolved(ok.parse().unwrap()),
                "{ok} should be allowed"
            );
        }
        for bad in [
            "198.18.0.1",
            "198.19.255.53",
            "127.0.0.1",
            "127.1.2.3",
            "169.254.169.254",
            "169.254.0.1",
            "0.0.0.0",
            "224.0.0.1",
            "239.255.255.250",
            "255.255.255.255",
            "::1",
            "fd00::5",
            "2001:db8::1",
            "::ffff:10.0.0.5",
        ] {
            assert!(
                !allowed_resolved(bad.parse().unwrap()),
                "{bad} should be refused"
            );
        }
    }

    #[test]
    fn pick_allowed_skips_refused_results_in_order() {
        let got = pick_allowed([
            sa("127.0.0.1:443"),
            sa("[fd00::5]:443"),
            sa("10.0.0.7:443"),
            sa("10.0.0.8:443"),
        ]);
        assert_eq!(got, Ok(sa("10.0.0.7:443")));
    }

    #[test]
    fn pick_allowed_refuses_when_nothing_is_allowed() {
        let got = pick_allowed([sa("169.254.169.254:80"), sa("127.0.0.1:80")]);
        assert_eq!(
            got,
            Err(ResolveError::NoAllowedAddress(vec![
                "169.254.169.254".parse().unwrap(),
                "127.0.0.1".parse().unwrap()
            ]))
        );
        assert!(matches!(pick_allowed([]), Err(ResolveError::NoAllowedAddress(v)) if v.is_empty()));
    }

    #[test]
    fn names_and_literals() {
        assert!(is_name("db.internal.example"));
        assert!(!is_name("10.0.0.5"));
        assert!(!is_name("::1"));
    }

    #[tokio::test]
    async fn ip_literals_are_used_as_given() {
        // Unchanged behaviour for IP resources: no lookup, no filtering.
        assert_eq!(resolve_target("10.0.0.5", 22).await, Ok(sa("10.0.0.5:22")));
        assert_eq!(
            resolve_target("127.0.0.1", 22).await,
            Ok(sa("127.0.0.1:22"))
        );
    }

    #[tokio::test]
    async fn a_name_resolving_to_loopback_is_refused() {
        // `localhost` resolves to loopback on every host (from /etc/hosts).
        match resolve_target("localhost", 22).await {
            Err(ResolveError::NoAllowedAddress(addrs)) => {
                assert!(
                    !addrs.is_empty() && addrs.iter().all(|a| a.is_loopback()),
                    "{addrs:?}"
                )
            }
            other => panic!("localhost: {other:?}, want NoAllowedAddress"),
        }
    }

    #[tokio::test]
    async fn an_unknown_name_is_a_lookup_error() {
        // RFC 6761: .invalid never resolves.
        assert!(matches!(
            resolve_target("nope.zecurity.invalid", 22).await,
            Err(ResolveError::Lookup(_))
        ));
    }
}
