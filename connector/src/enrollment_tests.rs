use super::{ca_cert_url, http_base_url};

#[test]
fn http_base_bare_host_port_defaults_to_http() {
    assert_eq!(http_base_url("127.0.0.1:8080"), "http://127.0.0.1:8080");
    assert_eq!(
        http_base_url("controller.example.com:8080"),
        "http://controller.example.com:8080"
    );
}

#[test]
fn http_base_explicit_scheme_is_preserved() {
    assert_eq!(
        http_base_url("https://controller.zecurity.in"),
        "https://controller.zecurity.in"
    );
    assert_eq!(
        http_base_url("http://controller.zecurity.in:8080"),
        "http://controller.zecurity.in:8080"
    );
}

#[test]
fn http_base_trailing_slash_is_trimmed() {
    assert_eq!(
        http_base_url("https://controller.zecurity.in/"),
        "https://controller.zecurity.in"
    );
}

#[test]
fn bare_host_port_defaults_to_http() {
    assert_eq!(
        ca_cert_url("127.0.0.1:8080"),
        "http://127.0.0.1:8080/ca.crt"
    );
    assert_eq!(
        ca_cert_url("controller.example.com:8080"),
        "http://controller.example.com:8080/ca.crt"
    );
}

#[test]
fn explicit_scheme_is_preserved() {
    assert_eq!(
        ca_cert_url("https://controller.zecurity.in"),
        "https://controller.zecurity.in/ca.crt"
    );
    assert_eq!(
        ca_cert_url("http://controller.zecurity.in:8080"),
        "http://controller.zecurity.in:8080/ca.crt"
    );
}

#[test]
fn trailing_slash_is_trimmed() {
    assert_eq!(
        ca_cert_url("https://controller.zecurity.in/"),
        "https://controller.zecurity.in/ca.crt"
    );
}
