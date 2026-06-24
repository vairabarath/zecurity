pub const SPIFFE_GLOBAL_TRUST_DOMAIN: &str = "zecurity.in";

pub fn relay_spiffe_id(relay_id: &str) -> String {
    format!("spiffe://{SPIFFE_GLOBAL_TRUST_DOMAIN}/relay/{relay_id}")
}
