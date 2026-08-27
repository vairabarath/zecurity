// protocol.rs — the two-verb wire contract from ADR-023.
//
// Newline-delimited JSON, mirroring the client's own IPC (`client/src/ipc.rs`) so
// there is one framing convention in the codebase rather than two.
//
// The enum is deliberately closed and tiny. Anything the helper might be asked to do
// beyond these two verbs is, by construction, not expressible.

use serde::{Deserialize, Serialize};

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "verb", rename_all = "snake_case")]
pub enum Request {
    /// Replace the link's routing-only domain list and point it at `server`.
    ///
    /// REPLACES — never appends. A deleted resource must lose its route, and the only
    /// way to guarantee the link agrees with the registry is to send the whole list.
    /// An empty `domains` is therefore meaningful: "route nothing".
    Apply {
        iface: String,
        server: String,
        domains: Vec<String>,
    },
    /// Drop our configuration from the link. Idempotent: a no-op on a link that was
    /// never configured, or that no longer exists.
    Revert { iface: String },
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct Response {
    pub ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

impl Response {
    pub fn ok() -> Self {
        Self {
            ok: true,
            error: None,
        }
    }
    pub fn err(msg: impl Into<String>) -> Self {
        Self {
            ok: false,
            error: Some(msg.into()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn apply_round_trips() {
        let r = Request::Apply {
            iface: "zecurity0".into(),
            server: "127.0.0.1".into(),
            domains: vec!["~app.internal".into()],
        };
        let s = serde_json::to_string(&r).unwrap();
        assert_eq!(serde_json::from_str::<Request>(&s).unwrap(), r);
    }

    #[test]
    fn revert_round_trips() {
        let r = Request::Revert {
            iface: "zecurity0".into(),
        };
        let s = serde_json::to_string(&r).unwrap();
        assert_eq!(serde_json::from_str::<Request>(&s).unwrap(), r);
    }

    /// The contract is closed: a third verb is not expressible, so an attacker cannot
    /// reach behaviour the helper does not have.
    #[test]
    fn an_unknown_verb_does_not_deserialize() {
        for bad in [
            r#"{"verb":"set_global_dns","server":"8.8.8.8"}"#,
            r#"{"verb":"exec","cmd":"sh"}"#,
            r#"{"verb":"apply"}"#,
            r#"{}"#,
            "not json",
        ] {
            assert!(
                serde_json::from_str::<Request>(bad).is_err(),
                "{bad} must not parse"
            );
        }
    }

    #[test]
    fn a_failure_response_carries_its_reason() {
        let s = serde_json::to_string(&Response::err("nope")).unwrap();
        assert!(s.contains("\"ok\":false") && s.contains("nope"), "got {s}");
        // Success omits the field rather than sending null.
        assert_eq!(
            serde_json::to_string(&Response::ok()).unwrap(),
            r#"{"ok":true}"#
        );
    }
}
