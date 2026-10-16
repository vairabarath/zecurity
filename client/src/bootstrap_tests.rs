//! Certificate audit bug 1, client part: the client's trust bootstrap can be
//! hijacked. `/ca.crt` is fetched over plain HTTP with no pin and every
//! certificate in it is trusted, and the EnrollDevice reply's CAs are saved
//! unvalidated. Mallory, on the network path, serves her own CA and receives
//! the login (PKCE challenge, invite token, access token), or hands back her
//! own "Workspace CA". Logout downloads `/ca.crt` again and is just as open.
//!
//! The fix anchors the client in the platform root compiled into the client:
//! `/ca.crt` must be exactly one Intermediate issued by that root, only it is
//! trusted, the reply is validated before anything is saved, and logout
//! reuses the anchors saved at enrollment.

use std::sync::Arc;

use crate::grpc::client_v1::{EnrollDeviceRequest, InitiateAuthRequest};
use crate::test_controller::{endpoint, CallLog, Reply};
use crate::test_pki::{device_key_and_csr, AttackerPki, TestPki};

/// The network part of `login::run` without the browser: connect, initiate
/// auth, enroll a fresh key, build the device CA chain to persist.
async fn login_flow(conf: &crate::config::ClientConf, root_pem: &str) -> anyhow::Result<String> {
    let (mut grpc, trusted) = crate::login::connect_controller(conf, root_pem).await?;
    grpc.initiate_auth(InitiateAuthRequest {
        workspace_slug: conf.workspace.clone(),
        code_challenge: "pkce-challenge".into(),
        local_redirect_uri: "http://127.0.0.1:1/callback".into(),
    })
    .await?;
    let (_key, csr_pem) = device_key_and_csr();
    let enroll = grpc
        .enroll_device(EnrollDeviceRequest {
            access_token: "login-access-token".into(),
            csr_pem: csr_pem.clone(),
            device_name: "test-host".into(),
            os: "linux".into(),
        })
        .await?
        .into_inner();
    crate::login::device_ca_chain(&trusted, &enroll, &csr_pem)
}

fn received(calls: &CallLog) -> Vec<String> {
    calls.lock().unwrap().clone()
}

struct Pkis {
    pki: Arc<TestPki>,
    attacker: Arc<AttackerPki>,
}

fn pkis() -> Pkis {
    Pkis {
        pki: Arc::new(TestPki::new()),
        attacker: Arc::new(AttackerPki::new()),
    }
}

// ---- login --------------------------------------------------------------

#[tokio::test]
async fn genuine_login_saves_the_platform_chain() {
    let p = pkis();
    let ep = endpoint(
        &p.pki,
        &p.attacker,
        p.pki.intermediate_pem.clone(),
        p.pki.controller_identity(),
        Reply::Genuine,
    )
    .await;
    let chain = login_flow(&ep.conf, &p.pki.root_pem)
        .await
        .expect("genuine login");
    assert_eq!(chain, p.pki.device_ca_chain());
}

#[tokio::test]
#[ignore = "bug 1 (client): demonstrates the bootstrap hijack; un-ignore with the fix"]
async fn replaced_ca_cannot_intercept_the_login() {
    let p = pkis();
    // Mallory answers both the /ca.crt download and the gRPC connection.
    let mallory = endpoint(
        &p.pki,
        &p.attacker,
        p.attacker.root_pem.clone(),
        p.attacker.identity(),
        Reply::Forged,
    )
    .await;
    let result = login_flow(&mallory.conf, &p.pki.root_pem).await;
    assert!(result.is_err(), "login completed against Mallory");
    assert!(
        received(&mallory.calls).is_empty(),
        "Mallory received: {:?}",
        received(&mallory.calls)
    );
}

#[tokio::test]
#[ignore = "bug 1 (client): demonstrates the bootstrap hijack; un-ignore with the fix"]
async fn appended_ca_cannot_intercept_the_login() {
    let p = pkis();
    let body = format!("{}\n{}", p.pki.intermediate_pem, p.attacker.root_pem);
    let mallory = endpoint(
        &p.pki,
        &p.attacker,
        body,
        p.attacker.identity(),
        Reply::Forged,
    )
    .await;
    assert!(login_flow(&mallory.conf, &p.pki.root_pem).await.is_err());
    assert!(
        received(&mallory.calls).is_empty(),
        "Mallory received: {:?}",
        received(&mallory.calls)
    );
}

#[tokio::test]
#[ignore = "bug 1 (client): demonstrates the bootstrap hijack; un-ignore with the fix"]
async fn look_alike_intermediate_not_issued_by_the_root_is_refused() {
    let p = pkis();
    // Same name as the platform Intermediate, but issued by Mallory's root.
    let mallory = endpoint(
        &p.pki,
        &p.attacker,
        p.attacker.fake_intermediate_pem.clone(),
        p.attacker.identity_under_fake_intermediate(),
        Reply::Forged,
    )
    .await;
    assert!(login_flow(&mallory.conf, &p.pki.root_pem).await.is_err());
    assert!(
        received(&mallory.calls).is_empty(),
        "Mallory received: {:?}",
        received(&mallory.calls)
    );
}

async fn login_with_reply(reply: Reply) -> anyhow::Result<String> {
    let p = pkis();
    let ep = endpoint(
        &p.pki,
        &p.attacker,
        p.pki.intermediate_pem.clone(),
        p.pki.controller_identity(),
        reply,
    )
    .await;
    login_flow(&ep.conf, &p.pki.root_pem).await
}

#[tokio::test]
#[ignore = "bug 1 (client): demonstrates the bootstrap hijack; un-ignore with the fix"]
async fn forged_enroll_reply_is_refused() {
    let err = login_with_reply(Reply::Forged)
        .await
        .expect_err("Mallory's Workspace CA must not be accepted");
    assert!(
        format!("{err:#}").contains("enrollment reply rejected"),
        "{err:#}"
    );
}

#[tokio::test]
#[ignore = "bug 1 (client): demonstrates the bootstrap hijack; un-ignore with the fix"]
async fn enroll_reply_from_another_platform_is_refused() {
    assert!(login_with_reply(Reply::OtherPlatform).await.is_err());
}

#[tokio::test]
#[ignore = "bug 1 (client): demonstrates the bootstrap hijack; un-ignore with the fix"]
async fn enroll_reply_for_another_key_is_refused() {
    assert!(login_with_reply(Reply::OtherKey).await.is_err());
}

// ---- logout -------------------------------------------------------------

#[tokio::test]
async fn logout_revokes_against_the_genuine_controller() {
    let p = pkis();
    let ep = endpoint(
        &p.pki,
        &p.attacker,
        p.pki.intermediate_pem.clone(),
        p.pki.controller_identity(),
        Reply::Genuine,
    )
    .await;
    crate::cmd::logout::revoke_device(&ep.conf, "tok", "dev", &p.pki.device_ca_chain())
        .await
        .expect("revoke against the genuine controller");
    assert_eq!(received(&ep.calls), vec!["RevokeDevice tok".to_string()]);
}

#[tokio::test]
#[ignore = "bug 1 (client): demonstrates the bootstrap hijack; un-ignore with the fix"]
async fn logout_does_not_trust_a_downloaded_ca() {
    let p = pkis();
    let mallory = endpoint(
        &p.pki,
        &p.attacker,
        p.attacker.root_pem.clone(),
        p.attacker.identity(),
        Reply::Forged,
    )
    .await;
    let result = crate::cmd::logout::revoke_device(
        &mallory.conf,
        "logout-access-token",
        "dev",
        &p.pki.device_ca_chain(),
    )
    .await;
    assert!(result.is_err(), "revoke completed against Mallory");
    assert!(
        received(&mallory.calls).is_empty(),
        "Mallory received: {:?}",
        received(&mallory.calls)
    );
}

#[tokio::test]
#[ignore = "bug 1 (client): demonstrates the bootstrap hijack; un-ignore with the fix"]
async fn logout_uses_the_saved_anchors_not_the_download() {
    let p = pkis();
    // A hostile /ca.crt in front of the genuine controller: logout must not
    // need the download at all.
    let ep = endpoint(
        &p.pki,
        &p.attacker,
        p.attacker.root_pem.clone(),
        p.pki.controller_identity(),
        Reply::Genuine,
    )
    .await;
    crate::cmd::logout::revoke_device(&ep.conf, "tok", "dev", &p.pki.device_ca_chain())
        .await
        .expect("revoke with the saved anchors");
    assert_eq!(received(&ep.calls), vec!["RevokeDevice tok".to_string()]);
}

// ---- saved state from before the fix --------------------------------------

#[test]
fn saved_platform_chain_is_trusted() {
    let p = pkis();
    crate::daemon::check_saved_trust(&p.pki.device_ca_chain(), &p.pki.root_pem)
        .expect("a chain issued under the platform root");
}

#[test]
#[ignore = "bug 1 (client): demonstrates the bootstrap hijack; un-ignore with the fix"]
fn saved_chain_not_under_the_platform_root_is_refused() {
    let p = pkis();
    let other = TestPki::new();
    let forged = format!(
        "{}\n{}",
        p.attacker.workspace_ca_pem, p.attacker.fake_intermediate_pem
    );
    for chain in [forged, other.device_ca_chain(), String::new()] {
        let err = crate::daemon::check_saved_trust(&chain, &p.pki.root_pem)
            .expect_err("an unanchored saved chain must be refused");
        assert!(format!("{err:#}").contains("login again"), "{err:#}");
    }
}
