//! Connector-revocation timing, fixed at build time (design D13–D15).
//!
//! `build.rs` takes each value from the build environment, else
//! `connector/.env`, else the accepted default, checks its range (an
//! out-of-range value fails the build) and generates these constants. They
//! are not read from `connector.conf` at runtime. See `connector/.env.example`.
//! Consumed by Phase B2.

include!(concat!(env!("OUT_DIR"), "/authz_timing.rs"));

#[cfg(test)]
mod tests {
    use super::*;

    /// build.rs bounds P against its own copy of H; it must be the real H.
    #[test]
    fn build_h_matches_the_runtime_freshness_bound() {
        assert_eq!(
            H_MS_AT_BUILD,
            crate::control_stream::AUTHORIZATION_FRESHNESS.as_millis() as u64
        );
    }

    #[test]
    fn baked_values_respect_the_deadline_and_ranges() {
        assert!((500..=H_MS_AT_BUILD - 1_000).contains(&AUTHZ_REQUEST_PERIOD_MS));
        assert!(CONTROL_RECONNECT_BASE_MS >= 100);
        assert!(CONTROL_RECONNECT_BASE_MS <= CONTROL_RECONNECT_MAX_MS);
        assert!(CONTROL_RECONNECT_MAX_MS <= 60_000);
        assert!(CONTROL_RECONNECT_JITTER_PCT <= 50);
        assert!((1..=3_600).contains(&DISABLED_SHIELD_CLOSE_SECS));
    }
}
