use anyhow::{anyhow, Result};
use serde::{Deserialize, Serialize};

pub const MAX_MSG_SIZE: usize = 16 * 1024;

#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum HandshakeMsg {
    Register {
        connector_id: String,
        spiffe_id: String,
    },
    Lookup {
        connector_id: String,
    },
    Probe {
        connector_id: String,
        request_id: u64,
    },
}

#[derive(Debug, Serialize, Deserialize)]
pub struct ProbeResponse {
    pub request_id: u64,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct RelayAck {
    pub ok: bool,
    pub error: Option<String>,
}

pub fn encode_message<T: Serialize>(msg: &T) -> Result<Vec<u8>> {
    let body = serde_json::to_vec(msg)?;

    if body.len() > MAX_MSG_SIZE {
        return Err(anyhow!("message too large"));
    }

    let mut out = Vec::with_capacity(4 + body.len());

    out.extend_from_slice(&(body.len() as u32).to_be_bytes());
    out.extend_from_slice(&body);

    Ok(out)
}

pub fn decode_message<T: for<'a> Deserialize<'a>>(buf: &[u8]) -> Result<T> {
    if buf.len() > MAX_MSG_SIZE {
        return Err(anyhow!("message too large"));
    }

    Ok(serde_json::from_slice(buf)?)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn register_roundtrip() {
        let msg = HandshakeMsg::Register {
            connector_id: "abc".into(),
            spiffe_id: "spiffe://test/connector/abc".into(),
        };

        let encoded = encode_message(&msg).unwrap();

        let len = u32::from_be_bytes(encoded[0..4].try_into().unwrap()) as usize;

        let decoded: HandshakeMsg = decode_message(&encoded[4..4 + len]).unwrap();

        match decoded {
            HandshakeMsg::Register { connector_id, .. } => {
                assert_eq!(connector_id, "abc");
            }
            _ => panic!("wrong message"),
        }
    }
}
