//! PENDING-14 F3: fake addresses for DNS-name resources.
//!
//! A resource whose address is a DNS name gets a fake IPv4 from
//! `198.18.0.0/16` (RFC 2544, F-7). Everything downstream of the parse points
//! (capture flows, table-105 routes, smoltcp listeners, the transport map) is
//! keyed on `(Ipv4Addr, port)` and works unchanged on the fake address. Only
//! the tunnel request maps it back: the connector authorizes and resolves the
//! **name** (PENDING-14 §4.3).
//!
//! A name's address is **derived from the name** (FNV-1a 64 of the name into
//! the pool, PENDING-14 F-9), so it is the same after a daemon restart, an
//! upgrade or a reboot: an application that cached it reaches the same
//! resource. Within one daemon run an address is never reused for another
//! name, so an old connection can never be redirected. Two names that hash
//! to the same slot are separated by probing to the next free address; for
//! such a pair, which one keeps the slot depends on the order the daemon
//! first sees them (the ACL order, normally unchanged across restarts).
//!
//! The DNS answerer (`dns.rs`) answers only names in the **active** set (the
//! names in the currently applied ACL); a removed name keeps its address but
//! stops resolving.

use std::collections::{BTreeSet, HashMap};
use std::net::{IpAddr, Ipv4Addr};
use std::sync::{Mutex, OnceLock};

/// First and last fake address handed out (198.18.0.1 … 198.18.255.254).
const POOL_FIRST: u32 = u32::from_be_bytes([198, 18, 0, 1]);
const POOL_LAST: u32 = u32::from_be_bytes([198, 18, 255, 254]);

/// The address the DNS answerer listens on, outside the name pool (F-7).
pub const DNS_RESPONDER_ADDR: Ipv4Addr = Ipv4Addr::new(198, 19, 255, 53);

/// Whether `ip` is in `198.18.0.0/15`, the range reserved for this client's
/// fake addresses and DNS answerer.
pub fn in_reserved_range(ip: Ipv4Addr) -> bool {
    let o = ip.octets();
    o[0] == 198 && (o[1] == 18 || o[1] == 19)
}

/// Whether `address` is a DNS name this client can map: the controller's rule
/// (`resource.NormalizeHost`): lowercase LDH labels of 1–63, at least two, at
/// most 253 characters, a letter in the last label. The controller stores
/// names normalised, so no case folding is done here.
pub fn is_resource_name(address: &str) -> bool {
    if address.is_empty() || address.len() > 253 || address.parse::<IpAddr>().is_ok() {
        return false;
    }
    let labels: Vec<&str> = address.split('.').collect();
    if labels.len() < 2 {
        return false;
    }
    let label_ok = |l: &&str| {
        !l.is_empty()
            && l.len() <= 63
            && !l.starts_with('-')
            && !l.ends_with('-')
            && l.bytes()
                .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == b'-')
    };
    labels.iter().all(label_ok)
        && labels
            .last()
            .unwrap()
            .bytes()
            .any(|c| c.is_ascii_lowercase())
}

/// Number of addresses in the pool.
const POOL_SIZE: u32 = POOL_LAST - POOL_FIRST + 1;

/// The pool slot a name hashes to: FNV-1a 64 of its bytes. **Never change
/// this function**: the addresses it yields must stay the same across client
/// versions (applications and resolvers cache them). `stable_addresses_are_pinned`
/// fails if it changes.
fn home_slot(name: &str) -> u32 {
    let mut h: u64 = 0xcbf2_9ce4_8422_2325;
    for b in name.as_bytes() {
        h ^= u64::from(*b);
        h = h.wrapping_mul(0x0000_0100_0000_01b3);
    }
    (h % u64::from(POOL_SIZE)) as u32
}

#[derive(Default)]
pub struct NameMap {
    by_name: HashMap<String, Ipv4Addr>,
    by_ip: HashMap<Ipv4Addr, String>,
    active: BTreeSet<String>,
    /// Real (IP-literal) resource addresses seen inside the reserved range.
    /// While any exists, names are not mapped: a fake address could shadow a
    /// real resource (PENDING-14 §4.7).
    conflicts: BTreeSet<Ipv4Addr>,
}

impl NameMap {
    /// The fake address for `name`: its hashed slot (`home_slot`), or the
    /// next free one if another name already holds that slot. None when the
    /// name is invalid, the pool is full, or the reserved range is in use by
    /// a real resource.
    pub fn fake_ip_for(&mut self, name: &str) -> Option<Ipv4Addr> {
        if !is_resource_name(name) || !self.conflicts.is_empty() {
            return None;
        }
        if let Some(ip) = self.by_name.get(name) {
            return Some(*ip);
        }
        let home = home_slot(name);
        let ip = (0..POOL_SIZE)
            .map(|i| Ipv4Addr::from(POOL_FIRST + (home + i) % POOL_SIZE))
            .find(|ip| !self.by_ip.contains_key(ip));
        let Some(ip) = ip else {
            tracing::warn!(name, "DNS name resources: fake address pool exhausted");
            return None;
        };
        if ip != Ipv4Addr::from(POOL_FIRST + home) {
            tracing::info!(name, %ip, "DNS name resources: hashed address taken by another name; using the next free one");
        }
        self.by_name.insert(name.to_string(), ip);
        self.by_ip.insert(ip, name.to_string());
        Some(ip)
    }

    /// The name behind a fake address, if any.
    pub fn name_for(&self, ip: Ipv4Addr) -> Option<&str> {
        self.by_ip.get(&ip).map(String::as_str)
    }

    /// Replace the set of names the DNS answerer resolves.
    pub fn set_active(&mut self, names: BTreeSet<String>) {
        self.active = names;
    }

    /// The answer for an A query: the fake address of an active name.
    pub fn answer_a(&self, name: &str) -> Option<Ipv4Addr> {
        if self.active.contains(name) && self.conflicts.is_empty() {
            self.by_name.get(name).copied()
        } else {
            None
        }
    }

    fn note_literal(&mut self, ip: Ipv4Addr) {
        if in_reserved_range(ip) && self.conflicts.insert(ip) {
            tracing::warn!(%ip, "a resource address is inside 198.18.0.0/15, the range reserved for DNS name resources; name resources are disabled");
        }
    }

    pub fn conflict(&self) -> Option<Ipv4Addr> {
        self.conflicts.iter().next().copied()
    }

    /// Forget the recorded reserved-range clashes. Called when the tunnel
    /// goes down: the next bring-up records only the clashes its ACL still
    /// has, so removing the clashing resource and restarting the tunnel
    /// re-enables names (no daemon restart needed).
    pub fn clear_conflicts(&mut self) {
        self.conflicts.clear();
    }
}

fn global() -> &'static Mutex<NameMap> {
    static MAP: OnceLock<Mutex<NameMap>> = OnceLock::new();
    MAP.get_or_init(|| Mutex::new(NameMap::default()))
}

/// Runs `f` on the process-wide map.
pub fn with<R>(f: impl FnOnce(&mut NameMap) -> R) -> R {
    f(&mut global().lock().unwrap_or_else(|e| e.into_inner()))
}

/// The IPv4 address a resource's `address` is captured on: the literal for an
/// IPv4 address, the fake address for a DNS name, None otherwise (IPv6, an
/// invalid string, an exhausted pool, a reserved-range conflict).
pub fn resource_ipv4(address: &str) -> Option<Ipv4Addr> {
    match address.parse::<IpAddr>() {
        Ok(IpAddr::V4(v4)) => {
            with(|m| m.note_literal(v4));
            Some(v4)
        }
        Ok(IpAddr::V6(_)) => None,
        Err(_) => with(|m| m.fake_ip_for(address)),
    }
}

/// The tunnel request `destination` for a captured `(ip, port)`: the name for
/// a fake address, the address itself otherwise.
pub fn destination_for(ip: Ipv4Addr) -> String {
    with(|m| m.name_for(ip).map(str::to_string)).unwrap_or_else(|| ip.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn names_follow_the_controller_rule() {
        for ok in [
            "db.internal",
            "db.internal.acme.com",
            "api-1.svc.cluster.local",
            "123.example",
            "x.y2k",
        ] {
            assert!(is_resource_name(ok), "{ok}");
        }
        for bad in [
            "",
            "localhost",
            "10.0.0.5",
            "::1",
            "*.acme.com",
            "DB.acme.com",
            "db_1.acme.com",
            "db..acme.com",
            "-db.acme.com",
            "db-.acme.com",
            "db.acme.123",
            "1.2.3.4.5",
            "db.acme.com.",
        ] {
            assert!(!is_resource_name(bad), "{bad}");
        }
    }

    #[test]
    fn fake_addresses_are_stable_distinct_and_in_the_pool() {
        let mut m = NameMap::default();
        let a = m.fake_ip_for("a.example").unwrap();
        let b = m.fake_ip_for("b.example").unwrap();
        assert_ne!(a, b);
        for ip in [a, b] {
            assert!(ip.octets()[0] == 198 && ip.octets()[1] == 18, "{ip}");
            assert!(
                ip != Ipv4Addr::new(198, 18, 0, 0) && ip != Ipv4Addr::new(198, 18, 255, 255),
                "{ip}"
            );
        }
        assert_eq!(m.fake_ip_for("a.example"), Some(a), "stable");
        assert_eq!(m.name_for(a), Some("a.example"));
        assert!(m.fake_ip_for("localhost").is_none());
        assert!(m.fake_ip_for("10.0.0.5").is_none());
    }

    #[test]
    fn an_address_is_never_reused_after_a_name_goes_inactive() {
        let mut m = NameMap::default();
        let a = m.fake_ip_for("a.example").unwrap();
        m.set_active(["a.example".to_string()].into());
        assert_eq!(m.answer_a("a.example"), Some(a));
        m.set_active(BTreeSet::new());
        assert_eq!(
            m.answer_a("a.example"),
            None,
            "inactive names don't resolve"
        );
        let b = m.fake_ip_for("b.example").unwrap();
        assert_ne!(a, b);
        assert_eq!(
            m.fake_ip_for("a.example"),
            Some(a),
            "a returning name keeps its address"
        );
    }

    #[test]
    fn stable_addresses_are_pinned() {
        // Computed independently (FNV-1a 64, mod 65534, from 198.18.0.1).
        // If this fails, addresses changed for every installed client.
        let mut m = NameMap::default();
        for (name, ip) in [
            ("db.internal", Ipv4Addr::new(198, 18, 230, 163)),
            ("192-168-1-164.nip.io", Ipv4Addr::new(198, 18, 130, 157)),
            ("a.example", Ipv4Addr::new(198, 18, 201, 129)),
        ] {
            assert_eq!(m.fake_ip_for(name), Some(ip), "{name}");
        }
    }

    #[test]
    fn a_restart_gives_the_same_addresses_whatever_the_order() {
        let names = [
            "db.internal",
            "web.acme.example",
            "x.y2k",
            "api-1.svc.cluster.local",
        ];
        let mut first = NameMap::default();
        let before: Vec<_> = names
            .iter()
            .map(|n| first.fake_ip_for(n).unwrap())
            .collect();
        let mut restarted = NameMap::default();
        for n in names.iter().rev() {
            restarted.fake_ip_for(n);
        }
        let after: Vec<_> = names
            .iter()
            .map(|n| restarted.fake_ip_for(n).unwrap())
            .collect();
        assert_eq!(before, after);
    }

    #[test]
    fn a_hash_collision_takes_the_next_free_address() {
        // n132.example and n369.example share slot 31761 (198.18.124.18).
        let mut m = NameMap::default();
        assert_eq!(
            m.fake_ip_for("n132.example"),
            Some(Ipv4Addr::new(198, 18, 124, 18))
        );
        assert_eq!(
            m.fake_ip_for("n369.example"),
            Some(Ipv4Addr::new(198, 18, 124, 19))
        );
        assert_eq!(
            m.name_for(Ipv4Addr::new(198, 18, 124, 19)),
            Some("n369.example")
        );
    }

    #[test]
    fn probing_wraps_and_the_pool_can_fill() {
        // w15805.example hashes to the last address; taken, it wraps to the first.
        let mut m = NameMap::default();
        m.by_ip
            .insert(Ipv4Addr::new(198, 18, 255, 254), "holder.example".into());
        assert_eq!(
            m.fake_ip_for("w15805.example"),
            Some(Ipv4Addr::new(198, 18, 0, 1))
        );

        let mut full = NameMap::default();
        for n in POOL_FIRST..=POOL_LAST {
            full.by_ip.insert(Ipv4Addr::from(n), String::new());
        }
        assert_eq!(full.fake_ip_for("late.example"), None, "pool full");
    }

    #[test]
    fn clearing_conflicts_re_enables_names() {
        let mut m = NameMap::default();
        m.note_literal(Ipv4Addr::new(198, 18, 7, 7));
        assert!(m.fake_ip_for("c.example").is_none());
        m.clear_conflicts();
        assert!(m.fake_ip_for("c.example").is_some());
    }

    #[test]
    fn a_real_resource_in_the_reserved_range_disables_names() {
        let mut m = NameMap::default();
        let a = m.fake_ip_for("a.example").unwrap();
        m.set_active(["a.example".to_string()].into());
        m.note_literal(Ipv4Addr::new(10, 0, 0, 1));
        assert!(m.conflict().is_none());
        m.note_literal(Ipv4Addr::new(198, 19, 1, 1));
        assert_eq!(m.conflict(), Some(Ipv4Addr::new(198, 19, 1, 1)));
        assert_eq!(m.answer_a("a.example"), None);
        assert_eq!(m.fake_ip_for("b.example"), None);
        let _ = a;
    }

    #[test]
    fn destination_maps_fake_addresses_back_to_names() {
        let ip = resource_ipv4("dest-test.example").unwrap();
        assert_eq!(destination_for(ip), "dest-test.example");
        assert_eq!(destination_for(Ipv4Addr::new(10, 0, 0, 1)), "10.0.0.1");
        assert_eq!(resource_ipv4("10.0.0.9"), Some(Ipv4Addr::new(10, 0, 0, 9)));
        assert_eq!(resource_ipv4("fd00::5"), None);
    }
}
