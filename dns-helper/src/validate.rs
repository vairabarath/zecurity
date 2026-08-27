// validate.rs — ADR-023 option C, the helper's whitelist.
//
// This module is the entire security value of the helper. It runs as **root**, so
// its job is to reject, not to trust: the calling daemon is unprivileged and its
// input is treated as hostile even though we expect it to be well-behaved.
//
// Zero dependencies on purpose. Everything here is pure and total, so the rules can
// be tested exhaustively without a socket, a root process, or systemd.
//
// The five invariants from ADR-023 that this file enforces mechanically:
//   1. Never modify global DNS          -> a domain of `~.` is rejected outright
//   2. Never modify another interface   -> `iface` must equal IFACE exactly
//   3. Only configure the Zecurity TUN  -> same check as 2
//   4. Route FQDNs individually         -> >= 2 labels required, no bare parent
//   5. Resolver keeps exact-match       -> Phase 11, not enforced here, but 4 is
//      what makes 5 safe: a parent domain would hand us names we must REFUSE.

use std::net::Ipv4Addr;

/// The one interface this helper will ever configure. Matches `client/src/tun.rs`,
/// which creates the device with this exact name.
pub const IFACE: &str = "zecurity0";

/// Synthetic address space — mirrors `client/src/registry.rs`
/// (`CGNAT_BASE` = 100.64.0.0, `CGNAT_PREFIX_LEN` = 10).
pub const SYNTHETIC_BASE: Ipv4Addr = Ipv4Addr::new(100, 64, 0, 0);
pub const SYNTHETIC_PREFIX_LEN: u8 = 10;

/// Upper bound on the domain list. A managed workspace will not have thousands of
/// name-addressed resources, and an unbounded list is a cheap way to make root do
/// unbounded work.
pub const MAX_DOMAINS: usize = 256;

/// Why a request was refused. Each variant names a distinct operator-visible cause —
/// collapsing them into one "invalid request" would make a misconfiguration and an
/// attack indistinguishable in the log.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Reject {
    /// Not our interface. The single most important rejection: it is what stops this
    /// helper being a general-purpose DNS setter for any link on the host.
    ForeignInterface { got: String },
    /// Not parseable as IPv4.
    ServerNotIpv4 { got: String },
    /// Parseable, but outside the two ranges we will ever point a link at.
    ServerOutOfRange { got: Ipv4Addr },
    /// Missing the `~` prefix, so it would be a *search* domain rather than
    /// routing-only — which changes unrelated lookups.
    DomainNotRoutingOnly { got: String },
    /// `~.` routes **every** name to us. Directly violates invariant 1.
    DomainIsRoot,
    /// A single label (e.g. `~internal`) is a parent domain: per invariant 4 it would
    /// capture sibling names we do not manage, and Phase 11 would REFUSE them with
    /// nowhere for resolved to fall back to.
    DomainNotFqdn { got: String },
    /// Malformed label: empty, over-long, or containing something that is not a
    /// hostname character.
    DomainMalformed { got: String, why: &'static str },
    /// Same domain twice — a sign the caller's list is not derived from the registry.
    DomainDuplicate { got: String },
    /// Too many domains.
    TooManyDomains { got: usize },
}

impl std::fmt::Display for Reject {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::ForeignInterface { got } => {
                write!(f, "refusing to configure interface {got:?}: only {IFACE} is permitted")
            }
            Self::ServerNotIpv4 { got } => write!(f, "server {got:?} is not an IPv4 address"),
            Self::ServerOutOfRange { got } => write!(
                f,
                "server {got} is neither loopback nor inside {SYNTHETIC_BASE}/{SYNTHETIC_PREFIX_LEN}"
            ),
            Self::DomainNotRoutingOnly { got } => write!(
                f,
                "domain {got:?} must be routing-only (prefixed with '~') so unmanaged lookups are unaffected"
            ),
            Self::DomainIsRoot => {
                write!(f, "domain '~.' would route ALL DNS to us — refused (invariant 1)")
            }
            Self::DomainNotFqdn { got } => write!(
                f,
                "domain {got:?} has a single label — route managed FQDNs individually, never a parent domain (invariant 4)"
            ),
            Self::DomainMalformed { got, why } => write!(f, "domain {got:?} is malformed: {why}"),
            Self::DomainDuplicate { got } => write!(f, "domain {got:?} appears more than once"),
            Self::TooManyDomains { got } => {
                write!(f, "{got} domains exceeds the maximum of {MAX_DOMAINS}")
            }
        }
    }
}

/// Invariants 2 and 3: this helper configures exactly one interface, by exact name.
pub fn validate_iface(iface: &str) -> Result<(), Reject> {
    if iface == IFACE {
        Ok(())
    } else {
        Err(Reject::ForeignInterface {
            got: iface.to_string(),
        })
    }
}

/// The only two places a managed name may be pointed at: loopback (where Phase 11's
/// responder listens) or inside the synthetic range (where it may listen in future,
/// as Twingate does). Anything else would let the caller aim the link's resolver at
/// an arbitrary host.
pub fn validate_server(server: &str) -> Result<Ipv4Addr, Reject> {
    let ip: Ipv4Addr = server.parse().map_err(|_| Reject::ServerNotIpv4 {
        got: server.to_string(),
    })?;
    if ip.is_loopback() || in_synthetic_range(ip) {
        Ok(ip)
    } else {
        Err(Reject::ServerOutOfRange { got: ip })
    }
}

fn in_synthetic_range(ip: Ipv4Addr) -> bool {
    let mask = u32::MAX
        .checked_shl(32 - SYNTHETIC_PREFIX_LEN as u32)
        .unwrap_or(0);
    (u32::from(ip) & mask) == (u32::from(SYNTHETIC_BASE) & mask)
}

/// Invariants 1 and 4.
pub fn validate_domain(domain: &str) -> Result<(), Reject> {
    let Some(rest) = domain.strip_prefix('~') else {
        return Err(Reject::DomainNotRoutingOnly {
            got: domain.to_string(),
        });
    };
    // `~.` and `~` both mean "everything". Either would hand us the whole namespace.
    let trimmed = rest.trim_end_matches('.');
    if trimmed.is_empty() {
        return Err(Reject::DomainIsRoot);
    }
    if trimmed.len() > 253 {
        return Err(Reject::DomainMalformed {
            got: domain.to_string(),
            why: "longer than 253 characters",
        });
    }
    let labels: Vec<&str> = trimmed.split('.').collect();
    if labels.len() < 2 {
        return Err(Reject::DomainNotFqdn {
            got: domain.to_string(),
        });
    }
    for l in &labels {
        if l.is_empty() {
            return Err(Reject::DomainMalformed {
                got: domain.to_string(),
                why: "empty label (consecutive or leading dot)",
            });
        }
        if l.len() > 63 {
            return Err(Reject::DomainMalformed {
                got: domain.to_string(),
                why: "label longer than 63 characters",
            });
        }
        // No wildcards: `pattern` is `reserved 14` on ACLEntry and unvalidated on the
        // wire, so a `*` here could only have come from somewhere it should not.
        if !l
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
        {
            return Err(Reject::DomainMalformed {
                got: domain.to_string(),
                why: "label contains a character that is not alphanumeric, '-' or '_'",
            });
        }
        if l.starts_with('-') || l.ends_with('-') {
            return Err(Reject::DomainMalformed {
                got: domain.to_string(),
                why: "label starts or ends with '-'",
            });
        }
    }
    Ok(())
}

/// Validate a whole `apply` request. Returns the parsed server on success.
///
/// Note this validates the FULL list every time, because `apply` **replaces** the
/// link's domain list rather than appending to it — otherwise a deleted resource
/// would leave its route behind and the list would drift from the registry.
pub fn validate_apply(iface: &str, server: &str, domains: &[String]) -> Result<Ipv4Addr, Reject> {
    validate_iface(iface)?;
    let ip = validate_server(server)?;
    if domains.len() > MAX_DOMAINS {
        return Err(Reject::TooManyDomains { got: domains.len() });
    }
    let mut seen: Vec<&str> = Vec::with_capacity(domains.len());
    for d in domains {
        validate_domain(d)?;
        let key = d.trim_end_matches('.');
        if seen.iter().any(|s| s.eq_ignore_ascii_case(key)) {
            return Err(Reject::DomainDuplicate { got: d.to_string() });
        }
        seen.push(key);
    }
    Ok(ip)
}

/// `revert` needs only the interface check. An empty domain list on a link that was
/// never configured is a no-op, so this is idempotent by construction.
pub fn validate_revert(iface: &str) -> Result<(), Reject> {
    validate_iface(iface)
}

#[cfg(test)]
mod tests {
    use super::*;

    // ── invariants 2 + 3: only our own interface ────────────────────────────

    #[test]
    fn only_the_zecurity_tun_is_accepted() {
        assert!(validate_iface("zecurity0").is_ok());
    }

    /// The single most important rejection in this file: without it the helper is a
    /// general-purpose "set DNS on any link" service running as root.
    #[test]
    fn every_other_interface_is_refused() {
        for bad in [
            "eth0",
            "enp2s0",
            "lo",
            "wlan0",
            "docker0",
            "zecurity1",
            "ZECURITY0",
            "zecurity0 ",
            " zecurity0",
            "zecurity0\n",
            "",
            "..",
            "../eth0",
            "zecurity0;eth0",
        ] {
            assert_eq!(
                validate_iface(bad),
                Err(Reject::ForeignInterface {
                    got: bad.to_string()
                }),
                "{bad:?} must be refused"
            );
        }
    }

    // ── server range ────────────────────────────────────────────────────────

    #[test]
    fn loopback_and_synthetic_servers_are_accepted() {
        // Phase 11 listens on 127.0.0.1; Task 1 proved resolved accepts it per-link.
        assert!(validate_server("127.0.0.1").is_ok());
        assert!(validate_server("127.0.0.53").is_ok());
        // Room for the responder to move onto the TUN address later, as Twingate does.
        assert!(validate_server("100.64.0.1").is_ok());
        assert!(validate_server("100.127.255.254").is_ok());
    }

    #[test]
    fn a_server_outside_both_ranges_is_refused() {
        for bad in [
            "8.8.8.8",
            "1.1.1.1",
            "192.168.1.1",
            "10.0.0.1",
            "100.63.255.255",
            "100.128.0.0",
        ] {
            let got = validate_server(bad);
            assert!(
                matches!(got, Err(Reject::ServerOutOfRange { .. })),
                "{bad} must be out of range, got {got:?}"
            );
        }
    }

    #[test]
    fn a_non_ipv4_server_is_refused() {
        for bad in [
            "::1",
            "localhost",
            "",
            "127.0.0.1:53",
            "not-an-ip",
            "999.1.1.1",
        ] {
            assert!(
                matches!(validate_server(bad), Err(Reject::ServerNotIpv4 { .. })),
                "{bad:?} must not parse as IPv4"
            );
        }
    }

    /// The /10 boundary, checked at the edges rather than in the middle.
    #[test]
    fn the_synthetic_range_boundary_is_exact() {
        assert!(
            validate_server("100.64.0.0").is_ok(),
            "first address in /10"
        );
        assert!(
            validate_server("100.127.255.255").is_ok(),
            "last address in /10"
        );
        assert!(
            validate_server("100.63.255.255").is_err(),
            "one below the /10"
        );
        assert!(validate_server("100.128.0.0").is_err(), "one above the /10");
    }

    // ── invariant 1: never route everything ─────────────────────────────────

    /// `~.` is the whole DNS namespace. Accepting it would make every lookup on the
    /// host depend on our responder — exactly what invariant 1 forbids.
    #[test]
    fn routing_the_root_zone_is_refused() {
        for bad in ["~.", "~", "~..", "~..."] {
            assert_eq!(validate_domain(bad), Err(Reject::DomainIsRoot), "{bad:?}");
        }
    }

    // ── invariant 4: individual FQDNs, never parents ────────────────────────

    #[test]
    fn an_individual_fqdn_is_accepted() {
        for ok in [
            "~fqdn-test.internal",
            "~fqdn-test.internal.",
            "~app.corp.example.com",
            "~a.b",
            "~_service.internal",
        ] {
            assert!(
                validate_domain(ok).is_ok(),
                "{ok:?} should be accepted: {:?}",
                validate_domain(ok)
            );
        }
    }

    /// A single label is a parent domain. Task 1 showed `~domain` captures the whole
    /// SUBTREE, so `~internal` would hand us every `*.internal` name — Phase 11 would
    /// REFUSE the unmanaged ones and, because the route is link-scoped, resolved has
    /// nowhere else to try. That breaks names we do not manage.
    #[test]
    fn a_bare_parent_domain_is_refused() {
        for bad in ["~internal", "~local", "~corp", "~internal."] {
            assert_eq!(
                validate_domain(bad),
                Err(Reject::DomainNotFqdn {
                    got: bad.to_string()
                }),
                "{bad:?} is a parent domain and must be refused"
            );
        }
    }

    #[test]
    fn a_search_domain_without_the_tilde_is_refused() {
        for bad in ["fqdn-test.internal", "app.corp.com", ".internal"] {
            assert!(
                matches!(
                    validate_domain(bad),
                    Err(Reject::DomainNotRoutingOnly { .. })
                ),
                "{bad:?} must require the '~' prefix"
            );
        }
    }

    #[test]
    fn wildcards_and_junk_are_refused() {
        for bad in [
            "~*.internal",
            "~app .internal",
            "~app\n.internal",
            "~app;.internal",
            "~app/.internal",
            "~app$.internal",
            "~app..internal",
            "~-app.internal",
            "~app-.internal",
            "~app.internal/../etc",
        ] {
            assert!(validate_domain(bad).is_err(), "{bad:?} must be refused");
        }
    }

    #[test]
    fn over_long_labels_and_names_are_refused() {
        let long_label = format!("~{}.internal", "a".repeat(64));
        assert!(matches!(
            validate_domain(&long_label),
            Err(Reject::DomainMalformed { .. })
        ));
        let long_name = format!("~{}.internal", vec!["a".repeat(60); 5].join("."));
        assert!(matches!(
            validate_domain(&long_name),
            Err(Reject::DomainMalformed { .. })
        ));
    }

    // ── whole-request validation ────────────────────────────────────────────

    fn d(v: &[&str]) -> Vec<String> {
        v.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn a_well_formed_apply_is_accepted() {
        let ip = validate_apply(
            "zecurity0",
            "127.0.0.1",
            &d(&["~fqdn-test.internal", "~other.internal"]),
        )
        .expect("should be accepted");
        assert_eq!(ip, Ipv4Addr::new(127, 0, 0, 1));
    }

    #[test]
    fn an_empty_domain_list_is_accepted_it_means_route_nothing() {
        // This is how the daemon expresses "no name-addressed resources any more"
        // without a separate verb — `apply` REPLACES the list.
        assert!(validate_apply("zecurity0", "127.0.0.1", &[]).is_ok());
    }

    #[test]
    fn one_bad_domain_rejects_the_whole_request() {
        // Partial application would leave the link in a state neither side intended.
        let r = validate_apply(
            "zecurity0",
            "127.0.0.1",
            &d(&["~fqdn-test.internal", "~internal"]),
        );
        assert!(matches!(r, Err(Reject::DomainNotFqdn { .. })));
    }

    #[test]
    fn duplicate_domains_are_refused_case_insensitively() {
        for pair in [
            ["~app.internal", "~app.internal"],
            ["~app.internal", "~APP.INTERNAL"],
            ["~app.internal", "~app.internal."],
        ] {
            assert!(
                matches!(
                    validate_apply("zecurity0", "127.0.0.1", &d(&pair)),
                    Err(Reject::DomainDuplicate { .. })
                ),
                "{pair:?} should be a duplicate"
            );
        }
    }

    #[test]
    fn an_unbounded_domain_list_is_refused() {
        let many: Vec<String> = (0..MAX_DOMAINS + 1)
            .map(|i| format!("~h{i}.internal"))
            .collect();
        assert_eq!(
            validate_apply("zecurity0", "127.0.0.1", &many),
            Err(Reject::TooManyDomains {
                got: MAX_DOMAINS + 1
            })
        );
    }

    /// Interface is checked FIRST, before anything else is even parsed — so a request
    /// aimed at someone else's link is refused on identity, not on its contents.
    #[test]
    fn the_interface_is_checked_before_the_payload() {
        let r = validate_apply("eth0", "8.8.8.8", &d(&["~."]));
        assert!(
            matches!(r, Err(Reject::ForeignInterface { .. })),
            "expected the interface rejection to win, got {r:?}"
        );
    }

    #[test]
    fn revert_only_checks_the_interface_and_is_idempotent() {
        assert!(validate_revert("zecurity0").is_ok());
        assert!(validate_revert("zecurity0").is_ok(), "must be repeatable");
        assert!(validate_revert("eth0").is_err());
    }

    #[test]
    fn rejections_render_a_reason_an_operator_can_act_on() {
        let msg = validate_iface("eth0").unwrap_err().to_string();
        assert!(
            msg.contains("eth0") && msg.contains("zecurity0"),
            "got: {msg}"
        );
        let msg = validate_domain("~internal").unwrap_err().to_string();
        assert!(msg.contains("invariant 4"), "got: {msg}");
    }
}
