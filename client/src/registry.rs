// registry.rs — Sprint 16 Phase 9.1 (PENDING-14 Stage 2)
//
// The client-local binding registry: hostname → synthetic IP → resource_id.
//
// WHY THE CLIENT OWNS THIS: only the client can see collisions between the
// synthetic CIDR and the user's actual network — their LAN, a coffee-shop
// Wi-Fi, another VPN already using CGNAT space. The controller has no view of
// any of that, so it never allocates, stores, or sees a synthetic IP.
//
// WHY IT IS A REGISTRY AND NOT A CACHE: the reverse mapping decides **which
// identity the client asserts** on the tunnel handshake. If a synthetic IP is
// silently remapped to a different resource across a restart, the client
// asserts the wrong resource_id, the connector authorizes that assertion
// correctly, and dials the WRONG RESOURCE. There is no layer below this that
// can catch it — the connector cannot know the client meant something else.
// That is why bindings are durable, why released addresses are quarantined
// before reuse, and why an identity change allocates a fresh address rather
// than rebinding an existing one.

use std::collections::{BTreeMap, HashMap, HashSet};
use std::net::Ipv4Addr;

use serde::{Deserialize, Serialize};

/// How long a released synthetic IP is withheld before it may be handed to a
/// different resource.
///
/// The risk being bounded is an app (or an OS DNS cache, once Stage 3 lands)
/// still holding a synthetic IP for a resource that has since been deleted. If
/// that address is immediately reissued, the stale holder silently reaches a
/// different resource. An hour is far longer than the 30–60s TTLs Stage 3 plans
/// and costs nothing but address space, of which a /22 has plenty.
pub const QUARANTINE_SECS: i64 = 3600;

/// Synthetic space is carved out of CGNAT (RFC 6598, 100.64.0.0/10) per sprint
/// decision #5. We take a /22 — 1024 addresses — which is far more than the
/// per-client resource counts in play and keeps the search for a
/// collision-free block cheap.
const SYNTHETIC_PREFIX_LEN: u8 = 22;
const CGNAT_BASE: u32 = 0x6440_0000; // 100.64.0.0
const CGNAT_PREFIX_LEN: u8 = 10;

/// An IPv4 network, as (base address, prefix length). A local type so this
/// module needs no CIDR crate; the only operations required are containment
/// and overlap.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Net {
    pub base: Ipv4Addr,
    pub prefix_len: u8,
}

impl Net {
    pub fn new(base: Ipv4Addr, prefix_len: u8) -> Self {
        // Normalise: mask off host bits so equality and overlap are meaningful
        // even when a caller passes an interface address rather than a network.
        let mask = prefix_mask(prefix_len);
        Self {
            base: Ipv4Addr::from(u32::from(base) & mask),
            prefix_len,
        }
    }

    pub fn contains(&self, addr: Ipv4Addr) -> bool {
        let mask = prefix_mask(self.prefix_len);
        (u32::from(addr) & mask) == u32::from(self.base)
    }

    /// True when the two networks share any address. One always contains the
    /// other's base when they overlap, because CIDR blocks nest.
    pub fn overlaps(&self, other: &Net) -> bool {
        self.contains(other.base) || other.contains(self.base)
    }

    fn first(&self) -> u32 {
        u32::from(self.base)
    }

    fn last(&self) -> u32 {
        u32::from(self.base) | !prefix_mask(self.prefix_len)
    }
}

impl std::fmt::Display for Net {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}/{}", self.base, self.prefix_len)
    }
}

fn prefix_mask(prefix_len: u8) -> u32 {
    if prefix_len == 0 {
        0
    } else {
        u32::MAX << (32 - prefix_len.min(32))
    }
}

/// One durable binding. Serialized into the encrypted state store (9.2), so
/// every field is `#[serde(default)]`-friendly and an older state file loads.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, Default)]
pub struct Binding {
    #[serde(default)]
    pub hostname: String,
    #[serde(default)]
    pub synthetic_ip: String,
    #[serde(default)]
    pub resource_id: String,
    #[serde(default)]
    pub allocated_at: i64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RegistryError {
    /// Every address in the synthetic CIDR is either live or still quarantined.
    Exhausted,
}

impl std::fmt::Display for RegistryError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Exhausted => write!(
                f,
                "synthetic address space exhausted (all addresses live or quarantined)"
            ),
        }
    }
}

/// The persisted form of the whole registry, handed to / from the state store.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct StoredRegistry {
    #[serde(default)]
    pub cidr_base: String,
    #[serde(default)]
    pub cidr_prefix_len: u8,
    #[serde(default)]
    pub bindings: Vec<Binding>,
    /// Released addresses and the instant they become reusable.
    #[serde(default)]
    pub quarantined: Vec<(String, i64)>,
}

pub struct BindingRegistry {
    cidr: Net,
    /// Live bindings keyed by synthetic IP. This is the direction that decides
    /// asserted identity, so it is the authoritative map; `by_hostname` is an
    /// index derived from it.
    live: BTreeMap<Ipv4Addr, Binding>,
    by_hostname: HashMap<String, Ipv4Addr>,
    /// Released addresses → the time they may be reused.
    quarantined: BTreeMap<Ipv4Addr, i64>,
    /// Allocation high-water mark. Addresses below this have been issued at
    /// some point; addresses at or above it are pristine. Preferring pristine
    /// addresses means reuse only happens under genuine pressure, which keeps
    /// the dangerous case rare rather than routine.
    next_fresh: u32,
}

impl BindingRegistry {
    // Read-side API with no production caller today.
    //
    // The wiring in 9.4b made the TRANSPORTS MAP the carrier of identity: the
    // daemon does the synthetic-IP → resource_id lookup once, at map-build time,
    // and net_stack reads `ResourceTarget.resource_id` from the key it matched. So
    // the runtime reverse lookup that `resolve` was written for never happens at
    // runtime — it is exercised by the tests that pin the registry's guarantees,
    // and it is the entry point Stage 3's DNS responder will need.
    //
    // `quarantined_rebuild` is the one to be honest about: 9.2 handles a corrupt
    // binding table by salvaging addresses during deserialization instead, so this
    // has NO caller and is not on any path. Kept because the salvage path cannot
    // cover a whole-envelope decrypt failure, which is where it would be used.
    #![allow(dead_code)]

    /// Empty registry over `cidr`. `.0` (network) and `.255`-style broadcast are
    /// not special here — the CIDR is routed to our own TUN and never appears on
    /// a broadcast domain — but we still skip the network address itself so the
    /// route and the first binding are never the same value in logs.
    pub fn new(cidr: Net) -> Self {
        Self {
            cidr,
            live: BTreeMap::new(),
            by_hostname: HashMap::new(),
            quarantined: BTreeMap::new(),
            next_fresh: cidr.first() + 1,
        }
    }

    pub fn cidr(&self) -> Net {
        self.cidr
    }

    pub fn len(&self) -> usize {
        self.live.len()
    }

    pub fn is_empty(&self) -> bool {
        self.live.is_empty()
    }

    /// Bind `hostname` to `resource_id`, returning its synthetic IP.
    ///
    /// **Stable**: the same (hostname, resource_id) always returns the same
    /// address, across restarts, for as long as the binding lives.
    ///
    /// **Identity changes never rebind.** If the hostname is already bound to a
    /// *different* resource_id, the old address is quarantined and a fresh one
    /// issued. Mutating the existing binding in place would make every holder
    /// of that address — an open socket, a cached DNS answer — silently start
    /// reaching a different resource. That is precisely the failure this
    /// registry exists to prevent, so it is handled as a release plus an
    /// allocation, never an update.
    pub fn bind(
        &mut self,
        hostname: &str,
        resource_id: &str,
        now: i64,
    ) -> Result<Ipv4Addr, RegistryError> {
        if let Some(&existing_ip) = self.by_hostname.get(hostname) {
            let same_resource = self
                .live
                .get(&existing_ip)
                .is_some_and(|b| b.resource_id == resource_id);
            if same_resource {
                return Ok(existing_ip);
            }
            self.release_ip(existing_ip, now);
        }

        let ip = self.allocate(now)?;
        self.live.insert(
            ip,
            Binding {
                hostname: hostname.to_string(),
                synthetic_ip: ip.to_string(),
                resource_id: resource_id.to_string(),
                allocated_at: now,
            },
        );
        self.by_hostname.insert(hostname.to_string(), ip);
        Ok(ip)
    }

    /// Reverse lookup — the security-critical direction.
    ///
    /// **Fail-closed by construction**: an address we did not issue, or one that
    /// has been released, returns `None`. Callers must treat `None` as "not a
    /// managed resource" and must never fall through to unmanaged passthrough
    /// on the strength of it.
    pub fn resolve(&self, ip: Ipv4Addr) -> Option<&Binding> {
        self.live.get(&ip)
    }

    pub fn ip_for_hostname(&self, hostname: &str) -> Option<Ipv4Addr> {
        self.by_hostname.get(hostname).copied()
    }

    pub fn bindings(&self) -> impl Iterator<Item = (&Ipv4Addr, &Binding)> {
        self.live.iter()
    }

    /// Drop bindings whose resource is no longer in the ACL, quarantining their
    /// addresses. Returns how many were released.
    ///
    /// Keyed on `resource_id`, not hostname: a resource renamed in the admin UI
    /// keeps its identity and should keep its address, while a resource that
    /// genuinely disappeared must give its address up.
    pub fn retain_resources(&mut self, active: &HashSet<String>, now: i64) -> usize {
        let stale: Vec<Ipv4Addr> = self
            .live
            .iter()
            .filter(|(_, b)| !active.contains(&b.resource_id))
            .map(|(ip, _)| *ip)
            .collect();
        let n = stale.len();
        for ip in stale {
            self.release_ip(ip, now);
        }
        n
    }

    fn release_ip(&mut self, ip: Ipv4Addr, now: i64) {
        if let Some(b) = self.live.remove(&ip) {
            // Only clear the forward index if it still points here; a rebind may
            // already have pointed the hostname at a new address.
            if self.by_hostname.get(&b.hostname) == Some(&ip) {
                self.by_hostname.remove(&b.hostname);
            }
        }
        self.quarantined.insert(ip, now + QUARANTINE_SECS);
    }

    /// Lowest pristine address first; only recycle a quarantined address when
    /// the pristine range is exhausted AND its quarantine has expired.
    fn allocate(&mut self, now: i64) -> Result<Ipv4Addr, RegistryError> {
        let last = self.cidr.last();
        let gw = gateway_addr(self.cidr);
        while self.next_fresh <= last {
            let candidate = Ipv4Addr::from(self.next_fresh);
            self.next_fresh += 1;
            // The gateway is the TUN's own address — see `gateway_addr`. Skipping
            // it HERE rather than by starting `next_fresh` higher states the
            // invariant, and survives `from_stored` recomputing the counter.
            if candidate == gw {
                continue;
            }
            if !self.live.contains_key(&candidate) && !self.quarantined.contains_key(&candidate) {
                return Ok(candidate);
            }
        }

        // Under pressure: recycle the lowest address whose quarantine has passed.
        let reusable = self
            .quarantined
            .iter()
            .find(|(ip, &until)| now >= until && **ip != gw)
            .map(|(ip, _)| *ip);
        match reusable {
            Some(ip) => {
                self.quarantined.remove(&ip);
                Ok(ip)
            }
            None => Err(RegistryError::Exhausted),
        }
    }

    // ── persistence (pairs with 9.2) ────────────────────────────────────────

    pub fn to_stored(&self) -> StoredRegistry {
        StoredRegistry {
            cidr_base: self.cidr.base.to_string(),
            cidr_prefix_len: self.cidr.prefix_len,
            bindings: self.live.values().cloned().collect(),
            quarantined: self
                .quarantined
                .iter()
                .map(|(ip, until)| (ip.to_string(), *until))
                .collect(),
        }
    }

    /// Rebuild from persisted state.
    ///
    /// `expected_cidr` is what this run selected. If the stored CIDR differs —
    /// the user's network changed and the old block now collides — every stored
    /// binding is outside the routable range and therefore meaningless. We
    /// return an empty registry rather than a mixture, because a half-valid
    /// binding table is exactly the state that produces a wrong assertion.
    ///
    /// Malformed entries are dropped individually and their addresses
    /// quarantined: we do not know what they used to mean, so they must not be
    /// reissued promptly.
    pub fn from_stored(stored: &StoredRegistry, expected_cidr: Net, now: i64) -> Self {
        let stored_cidr = stored
            .cidr_base
            .parse::<Ipv4Addr>()
            .ok()
            .map(|b| Net::new(b, stored.cidr_prefix_len));

        if stored_cidr != Some(expected_cidr) {
            return Self::new(expected_cidr);
        }

        let mut reg = Self::new(expected_cidr);
        for b in &stored.bindings {
            let Ok(ip) = b.synthetic_ip.parse::<Ipv4Addr>() else {
                continue;
            };
            // Anything outside the CIDR, or lacking an identity, cannot be acted
            // on safely. Quarantine rather than silently reuse.
            if !expected_cidr.contains(ip) || b.resource_id.is_empty() || b.hostname.is_empty() {
                reg.quarantined.insert(ip, now + QUARANTINE_SECS);
                continue;
            }
            // A binding written by a build that allocated the gateway. Drop it so
            // the hostname is rebound to a routable address on the next sync;
            // without this, an already-deployed client keeps an unreachable
            // address forever and self-heals only on `logout`. Deliberately NOT
            // quarantined — the gateway is permanently reserved, and quarantine
            // would advertise it as reusable later.
            if ip == gateway_addr(expected_cidr) {
                continue;
            }
            // Duplicate address in the file: keep the first, quarantine the rest.
            if reg.live.contains_key(&ip) {
                reg.quarantined.insert(ip, now + QUARANTINE_SECS);
                continue;
            }
            reg.by_hostname.insert(b.hostname.clone(), ip);
            reg.live.insert(ip, b.clone());
        }
        for (ip, until) in &stored.quarantined {
            if let Ok(ip) = ip.parse::<Ipv4Addr>() {
                if !reg.live.contains_key(&ip) {
                    reg.quarantined.insert(ip, *until);
                }
            }
        }
        // Never hand out an address at or below anything already issued.
        let high = reg
            .live
            .keys()
            .chain(reg.quarantined.keys())
            .map(|ip| u32::from(*ip))
            .max();
        if let Some(h) = high {
            reg.next_fresh = reg.next_fresh.max(h + 1);
        }
        reg
    }

    /// A registry whose bindings are all unknown — used when the stored table is
    /// corrupt. Starting empty is right (we must not refuse to start), but the
    /// addresses that were in use are NOT free: something may still hold them.
    pub fn quarantined_rebuild(cidr: Net, previously_issued: &[Ipv4Addr], now: i64) -> Self {
        let mut reg = Self::new(cidr);
        for ip in previously_issued {
            if cidr.contains(*ip) {
                reg.quarantined.insert(*ip, now + QUARANTINE_SECS);
                reg.next_fresh = reg.next_fresh.max(u32::from(*ip) + 1);
            }
        }
        reg
    }
}

/// The TUN's own address inside a synthetic CIDR: the first host address.
///
/// `tun.rs` assigns this to `zecurity0` and `net_stack` uses it as smoltcp's
/// address and its AnyIP default-route gateway. Because the interface owns it,
/// the kernel installs `local <gw> dev zecurity0 table local`, and rule
/// `0: from all lookup local` is consulted BEFORE `49: from all fwmark 0x5a
/// lookup 105`. A binding on this address is therefore delivered to the local
/// host and never enters the tunnel — unreachable by construction.
///
/// It must never be handed out as a binding. Note this is *reserved*, not
/// *quarantined*: quarantine means "reusable after QUARANTINE_SECS", which is
/// exactly wrong here.
///
/// ⚠️ `tun.rs` and `net_stack.rs` currently hardcode `100.64.0.1` rather than
/// deriving it from the chosen CIDR. That is correct only while `select_cidr`
/// returns `100.64.0.0/22`; see the Phase 9 post-phase notes.
pub fn gateway_addr(cidr: Net) -> Ipv4Addr {
    Ipv4Addr::from(cidr.first() + 1)
}

/// Choose a synthetic /22 inside 100.64.0.0/10 that does not overlap anything
/// already present on this host.
///
/// CGNAT space is genuinely used in the wild — carrier networks and other VPN
/// clients both live there — so picking a fixed block would eventually route a
/// real destination into our TUN and black-hole it. `observed` should carry
/// every network this host can already reach: interface networks and route
/// destinations.
///
/// Returns `None` when the whole /10 is contested, which is a fail-closed
/// outcome: better to refuse name-addressed resources than to hijack traffic.
pub fn select_cidr(observed: &[Net]) -> Option<Net> {
    let block_size = 1u32 << (32 - SYNTHETIC_PREFIX_LEN);
    let cgnat = Net::new(Ipv4Addr::from(CGNAT_BASE), CGNAT_PREFIX_LEN);
    let mut base = cgnat.first();
    while base + block_size - 1 <= cgnat.last() {
        let candidate = Net::new(Ipv4Addr::from(base), SYNTHETIC_PREFIX_LEN);
        if !observed.iter().any(|o| o.overlaps(&candidate)) {
            return Some(candidate);
        }
        base += block_size;
    }
    None
}

#[cfg(test)]
mod tests {
    use super::*;

    const T0: i64 = 1_700_000_000;

    fn cidr() -> Net {
        Net::new(Ipv4Addr::new(100, 64, 0, 0), 22)
    }

    fn reg() -> BindingRegistry {
        BindingRegistry::new(cidr())
    }

    fn net(s: &str, p: u8) -> Net {
        Net::new(s.parse().unwrap(), p)
    }

    // ── Net arithmetic ──────────────────────────────────────────────────────

    #[test]
    fn net_normalises_host_bits() {
        // Callers pass interface addresses, not tidy network bases.
        assert_eq!(net("192.168.1.57", 24).base, Ipv4Addr::new(192, 168, 1, 0));
    }

    #[test]
    fn net_contains_and_overlaps() {
        let a = net("100.64.0.0", 22);
        assert!(a.contains("100.64.3.255".parse().unwrap()));
        assert!(!a.contains("100.64.4.0".parse().unwrap()));
        assert!(a.overlaps(&net("100.64.0.0", 10)), "a /10 contains our /22");
        assert!(!a.overlaps(&net("100.64.4.0", 22)));
    }

    // ── stability: the acceptance-critical property ─────────────────────────

    #[test]
    fn same_hostname_and_resource_always_gets_the_same_ip() {
        let mut r = reg();
        let a = r.bind("db.internal", "res-1", T0).unwrap();
        let b = r.bind("db.internal", "res-1", T0 + 5).unwrap();
        assert_eq!(a, b);
        assert_eq!(r.len(), 1, "rebinding must not create a second binding");
    }

    /// **The regression test the phase file calls acceptance-critical.** A
    /// restart must not remap a synthetic IP to a different resource.
    #[test]
    fn restart_preserves_every_binding_exactly() {
        let mut r = reg();
        let a = r.bind("a.internal", "res-a", T0).unwrap();
        let b = r.bind("b.internal", "res-b", T0).unwrap();
        let c = r.bind("c.internal", "res-c", T0).unwrap();

        let restored = BindingRegistry::from_stored(&r.to_stored(), cidr(), T0 + 10);

        assert_eq!(restored.resolve(a).unwrap().resource_id, "res-a");
        assert_eq!(restored.resolve(b).unwrap().resource_id, "res-b");
        assert_eq!(restored.resolve(c).unwrap().resource_id, "res-c");
        assert_eq!(restored.ip_for_hostname("b.internal"), Some(b));
    }

    /// The failure this registry exists to prevent, stated as an assertion: after
    /// a restart, no address may answer with a resource it did not answer with
    /// before. A silent remap makes the client assert the wrong identity and the
    /// connector authorizes it correctly — nothing downstream can catch it.
    #[test]
    fn restart_never_remaps_an_ip_to_a_different_resource() {
        let mut r = reg();
        let mut before = Vec::new();
        for i in 0..25 {
            let host = format!("h{i}.internal");
            let rid = format!("res-{i}");
            before.push((r.bind(&host, &rid, T0).unwrap(), rid));
        }
        // A churn cycle: drop some, add others, then restart.
        let keep: HashSet<String> = (0..25)
            .filter(|i| i % 3 != 0)
            .map(|i| format!("res-{i}"))
            .collect();
        r.retain_resources(&keep, T0);
        for i in 25..35 {
            r.bind(&format!("h{i}.internal"), &format!("res-{i}"), T0 + 1)
                .unwrap();
        }

        let restored = BindingRegistry::from_stored(&r.to_stored(), cidr(), T0 + 2);
        for (ip, rid) in before {
            if let Some(b) = restored.resolve(ip) {
                assert_eq!(
                    b.resource_id, rid,
                    "{ip} answered with {} after restart, was {rid} — a silent remap",
                    b.resource_id
                );
            }
        }
    }

    // ── identity changes ────────────────────────────────────────────────────

    /// A hostname repointed at a different resource must NOT keep its address.
    /// Anything still holding the old IP — an open socket, a cached DNS answer —
    /// would otherwise start reaching a different resource with no signal.
    #[test]
    fn rebinding_a_hostname_to_a_new_resource_issues_a_fresh_ip() {
        let mut r = reg();
        let old = r.bind("app.internal", "res-old", T0).unwrap();
        let new = r.bind("app.internal", "res-new", T0 + 1).unwrap();

        assert_ne!(old, new, "an identity change must not reuse the address");
        assert!(
            r.resolve(old).is_none(),
            "the old address must stop resolving immediately"
        );
        assert_eq!(r.resolve(new).unwrap().resource_id, "res-new");
        assert_eq!(r.ip_for_hostname("app.internal"), Some(new));
    }

    // ── reverse lookup is fail-closed ───────────────────────────────────────

    #[test]
    fn unknown_and_released_addresses_do_not_resolve() {
        let mut r = reg();
        let ip = r.bind("a.internal", "res-a", T0).unwrap();
        assert!(r.resolve("100.64.3.99".parse().unwrap()).is_none());
        assert!(r.resolve("10.0.0.1".parse().unwrap()).is_none());

        r.retain_resources(&HashSet::new(), T0);
        assert!(
            r.resolve(ip).is_none(),
            "a released address must not resolve to its old resource"
        );
    }

    // ── quarantine ──────────────────────────────────────────────────────────

    /// Scenario check: a freed address is not handed to the next resource that asks.
    ///
    /// ⚠️ Note what actually delivers this, because the obvious reading is wrong.
    /// It is **pristine-first allocation**, not quarantine — deleting the
    /// `quarantined.insert` in `release_ip` leaves this test green, because
    /// `next_fresh` has already moved past the freed address (verified by
    /// reverting it). Quarantine only bites under exhaustion, and its real guard
    /// is `exhaustion_respects_quarantine_then_recycles_after_it_expires`.
    /// Kept as a scenario test, renamed for what it observes rather than for the
    /// mechanism it looked like it was testing.
    #[test]
    fn a_freed_address_is_not_handed_to_the_next_requester() {
        let mut r = reg();
        let freed = r.bind("gone.internal", "res-gone", T0).unwrap();
        r.retain_resources(&HashSet::new(), T0);

        for i in 0..50 {
            let ip = r
                .bind(&format!("n{i}.internal"), &format!("res-n{i}"), T0 + 1)
                .unwrap();
            assert_ne!(ip, freed, "a freed address was reissued to a new resource");
        }
    }

    /// Exhaustion is where quarantine actually bites: with no pristine addresses
    /// left, a still-quarantined address must NOT be handed out, even though that
    /// means failing the allocation.
    #[test]
    fn exhaustion_respects_quarantine_then_recycles_after_it_expires() {
        // A /30 gives 4 addresses; we skip BOTH the network address and the
        // reserved gateway (`gateway_addr`), leaving 2 usable — .2 and .3.
        let small = Net::new(Ipv4Addr::new(100, 64, 0, 0), 30);
        let mut r = BindingRegistry::new(small);
        for i in 0..2 {
            r.bind(&format!("h{i}"), &format!("res-{i}"), T0).unwrap();
        }
        // Free one; it is quarantined.
        let keep: HashSet<String> = ["res-1".into()].into_iter().collect();
        r.retain_resources(&keep, T0);

        assert_eq!(
            r.bind("new", "res-new", T0 + 1),
            Err(RegistryError::Exhausted),
            "a quarantined address must not be reissued just because we are full"
        );

        // Once quarantine expires it becomes available again.
        let ip = r.bind("new", "res-new", T0 + QUARANTINE_SECS + 1).unwrap();
        assert!(small.contains(ip));
    }

    // ── the reserved gateway (Phase 9 post-phase fix) ───────────────────────
    //
    // The TUN owns `gateway_addr(cidr)`, so the kernel routes it via the `local`
    // table (rule 0) BEFORE the fwmark rule (49). A binding there is delivered to
    // the local host and never enters the tunnel. This was live-only: the whole
    // data plane was installed correctly and the resource was still unreachable.

    #[test]
    fn the_first_binding_is_never_the_tunnel_gateway() {
        let mut r = reg();
        let ip = r.bind("app.internal", "res-a", T0).unwrap();
        assert_ne!(
            ip,
            gateway_addr(cidr()),
            "the first binding took the TUN's own address — unreachable by construction"
        );
        assert_eq!(ip, "100.64.0.2".parse::<Ipv4Addr>().unwrap());
    }

    #[test]
    fn no_allocation_ever_returns_the_gateway() {
        let mut r = reg();
        let gw = gateway_addr(cidr());
        for i in 0..64 {
            let ip = r.bind(&format!("h{i}.internal"), &format!("res-{i}"), T0).unwrap();
            assert_ne!(ip, gw, "allocation {i} handed out the gateway");
        }
    }

    /// Recycling under pressure must not reach the gateway either.
    #[test]
    fn the_gateway_is_not_recycled_under_pressure() {
        let small = Net::new(Ipv4Addr::new(100, 64, 0, 0), 30);
        let mut r = BindingRegistry::new(small);
        // Pretend a previous build quarantined the gateway.
        r.quarantined.insert(gateway_addr(small), T0);
        let ip = r.bind("a.internal", "res-a", T0 + QUARANTINE_SECS + 1).unwrap();
        assert_ne!(ip, gateway_addr(small));
    }

    /// An already-deployed client carries a stored binding on the gateway. It must
    /// be discarded so the hostname is rebound to a routable address, rather than
    /// self-healing only on `logout`.
    #[test]
    fn a_stored_binding_on_the_gateway_is_discarded_and_rebound() {
        let c = cidr();
        let gw = gateway_addr(c);
        let stored = StoredRegistry {
            cidr_base: c.base.to_string(),
            cidr_prefix_len: c.prefix_len,
            bindings: vec![Binding {
                hostname: "app.internal".into(),
                synthetic_ip: gw.to_string(),
                resource_id: "res-a".into(),
                allocated_at: T0,
            }],
            quarantined: Default::default(),
        };
        let mut r = BindingRegistry::from_stored(&stored, c, T0);
        assert!(r.resolve(gw).is_none(), "the gateway binding must not be restored");
        // And the hostname gets a routable address on the next sync.
        let ip = r.bind("app.internal", "res-a", T0).unwrap();
        assert_ne!(ip, gw);
        assert!(c.contains(ip));
        // Reserved, NOT quarantined — quarantine would advertise it as reusable.
        assert!(!r.quarantined.contains_key(&gw));
    }

    #[test]
    fn pristine_addresses_are_preferred_over_recycling() {
        let mut r = reg();
        let first = r.bind("a", "res-a", T0).unwrap();
        r.retain_resources(&HashSet::new(), T0);
        // Long after quarantine expired, a fresh bind should still take a NEW
        // address — recycling is for pressure, not the common path.
        let next = r.bind("b", "res-b", T0 + QUARANTINE_SECS * 10).unwrap();
        assert_ne!(next, first);
        assert!(u32::from(next) > u32::from(first));
    }

    // ── restore hardening ───────────────────────────────────────────────────

    #[test]
    fn a_stored_cidr_that_no_longer_matches_yields_an_empty_registry() {
        let mut r = reg();
        r.bind("a.internal", "res-a", T0).unwrap();
        let stored = r.to_stored();

        // This run picked a different block (the old one now collides).
        let moved = Net::new(Ipv4Addr::new(100, 68, 0, 0), 22);
        let restored = BindingRegistry::from_stored(&stored, moved, T0);
        assert!(
            restored.is_empty(),
            "bindings outside the routable CIDR are meaningless and must not be kept"
        );
    }

    #[test]
    fn malformed_stored_entries_are_dropped_and_their_addresses_quarantined() {
        let stored = StoredRegistry {
            cidr_base: "100.64.0.0".into(),
            cidr_prefix_len: 22,
            bindings: vec![
                Binding {
                    hostname: "ok.internal".into(),
                    synthetic_ip: "100.64.0.5".into(),
                    resource_id: "res-ok".into(),
                    allocated_at: T0,
                },
                // No resource_id — we cannot know what this address meant.
                Binding {
                    hostname: "bad.internal".into(),
                    synthetic_ip: "100.64.0.6".into(),
                    resource_id: String::new(),
                    allocated_at: T0,
                },
                // Outside the CIDR entirely.
                Binding {
                    hostname: "outside.internal".into(),
                    synthetic_ip: "10.0.0.9".into(),
                    resource_id: "res-outside".into(),
                    allocated_at: T0,
                },
            ],
            quarantined: vec![],
        };
        let r = BindingRegistry::from_stored(&stored, cidr(), T0);

        assert_eq!(r.len(), 1);
        assert_eq!(
            r.resolve("100.64.0.5".parse().unwrap())
                .unwrap()
                .resource_id,
            "res-ok"
        );
        assert!(r.resolve("100.64.0.6".parse().unwrap()).is_none());
        // And the dropped in-CIDR address is not immediately reissued.
        for i in 0..20 {
            let ip = r_bind(&mut r.clone_for_test(), i, T0);
            assert_ne!(ip, "100.64.0.6".parse::<Ipv4Addr>().unwrap());
        }
    }

    // small helpers for the loop above
    impl BindingRegistry {
        fn clone_for_test(&self) -> BindingRegistry {
            BindingRegistry::from_stored(&self.to_stored(), self.cidr(), T0)
        }
    }
    fn r_bind(r: &mut BindingRegistry, i: usize, now: i64) -> Ipv4Addr {
        r.bind(&format!("t{i}"), &format!("res-t{i}"), now).unwrap()
    }

    #[test]
    fn corrupt_table_rebuilds_empty_with_everything_quarantined() {
        let previously = vec!["100.64.0.1".parse().unwrap(), "100.64.0.2".parse().unwrap()];
        let mut r = BindingRegistry::quarantined_rebuild(cidr(), &previously, T0);
        assert!(r.is_empty(), "a corrupt table must not refuse to start");
        for i in 0..20 {
            let ip = r.bind(&format!("h{i}"), &format!("res-{i}"), T0).unwrap();
            assert!(
                !previously.contains(&ip),
                "{ip} was previously issued and must be quarantined, not reused"
            );
        }
    }

    // ── CIDR selection ──────────────────────────────────────────────────────

    #[test]
    fn select_cidr_avoids_networks_already_on_the_host() {
        // Another VPN is already using the bottom of CGNAT space.
        let observed = vec![net("100.64.0.0", 16), net("192.168.1.0", 24)];
        let picked = select_cidr(&observed).expect("a free block must exist");
        assert!(!observed.iter().any(|o| o.overlaps(&picked)));
        assert!(
            Net::new(Ipv4Addr::from(CGNAT_BASE), CGNAT_PREFIX_LEN).overlaps(&picked),
            "the chosen block must still be inside 100.64.0.0/10"
        );
    }

    #[test]
    fn select_cidr_takes_the_lowest_free_block_when_nothing_collides() {
        assert_eq!(select_cidr(&[]).unwrap(), net("100.64.0.0", 22));
    }

    /// Fail closed: refusing name-addressed resources beats hijacking traffic
    /// the host already has a legitimate route for.
    #[test]
    fn select_cidr_returns_none_when_the_whole_range_is_contested() {
        assert_eq!(select_cidr(&[net("100.64.0.0", 10)]), None);
    }
}
