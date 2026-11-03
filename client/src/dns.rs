//! PENDING-14 F3: the authoritative DNS answerer for DNS-name resources.
//!
//! systemd-resolved sends queries for the resource names routed to
//! `zecurity0` (`~name` routing domains) to `198.19.255.53:53/udp`, a socket
//! in the client's smoltcp stack. Answers (§4.3):
//!
//! - `A <active name>` → the name's fake address, TTL 30 s;
//! - any other type for an active name → NOERROR with no answer (NODATA), at
//!   once, so applications don't wait on IPv6;
//! - any other name (e.g. a subdomain of a routing domain) → NXDOMAIN;
//! - malformed or non-query packets → no reply.
//!
//! UDP only; answers are small, so TC is never set.

use std::net::Ipv4Addr;

pub const ANSWER_TTL_SECS: u32 = 30;

const TYPE_A: u16 = 1;
const CLASS_IN: u16 = 1;
const RCODE_NXDOMAIN: u16 = 3;
const RCODE_NOTIMP: u16 = 4;

/// Builds the reply to one DNS query, or None to drop it. `lookup` returns the
/// fake address of an active resource name (lowercase, no trailing dot).
pub fn answer(query: &[u8], lookup: impl Fn(&str) -> Option<Ipv4Addr>) -> Option<Vec<u8>> {
    if query.len() < 12 {
        return None;
    }
    let flags = u16::from_be_bytes([query[2], query[3]]);
    let qr = flags & 0x8000 != 0;
    let opcode = (flags >> 11) & 0xF;
    let qdcount = u16::from_be_bytes([query[4], query[5]]);
    if qr || qdcount != 1 {
        return None;
    }

    // One question: QNAME (uncompressed labels), QTYPE, QCLASS.
    let mut pos = 12;
    let mut labels: Vec<String> = Vec::new();
    loop {
        let len = *query.get(pos)? as usize;
        pos += 1;
        if len == 0 {
            break;
        }
        if len > 63 {
            return None; // compression pointers never appear in a query's QNAME
        }
        let label = query.get(pos..pos + len)?;
        labels.push(String::from_utf8(label.to_ascii_lowercase()).ok()?);
        pos += len;
    }
    let qtype = u16::from_be_bytes([*query.get(pos)?, *query.get(pos + 1)?]);
    let qclass = u16::from_be_bytes([*query.get(pos + 2)?, *query.get(pos + 3)?]);
    let question_end = pos + 4;
    let name = labels.join(".");

    let rd = flags & 0x0100;
    let (rcode, addr) = if opcode != 0 {
        (RCODE_NOTIMP, None)
    } else {
        match lookup(&name) {
            None => (RCODE_NXDOMAIN, None),
            Some(ip) if qtype == TYPE_A && qclass == CLASS_IN => (0, Some(ip)),
            Some(_) => (0, None), // NODATA
        }
    };

    let mut out = Vec::with_capacity(question_end + 16);
    out.extend_from_slice(&query[0..2]); // ID
    let reply_flags: u16 = 0x8000 | (opcode << 11) | 0x0400 /* AA */ | rd | rcode;
    out.extend_from_slice(&reply_flags.to_be_bytes());
    out.extend_from_slice(&1u16.to_be_bytes()); // QDCOUNT
    out.extend_from_slice(&(addr.is_some() as u16).to_be_bytes()); // ANCOUNT
    out.extend_from_slice(&0u16.to_be_bytes()); // NSCOUNT
    out.extend_from_slice(&0u16.to_be_bytes()); // ARCOUNT
    out.extend_from_slice(&query[12..question_end]);
    if let Some(ip) = addr {
        out.extend_from_slice(&0xC00Cu16.to_be_bytes()); // name: pointer to the question
        out.extend_from_slice(&TYPE_A.to_be_bytes());
        out.extend_from_slice(&CLASS_IN.to_be_bytes());
        out.extend_from_slice(&ANSWER_TTL_SECS.to_be_bytes());
        out.extend_from_slice(&4u16.to_be_bytes());
        out.extend_from_slice(&ip.octets());
    }
    Some(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn query(id: u16, name: &str, qtype: u16, extra_additional: bool) -> Vec<u8> {
        let mut q = Vec::new();
        q.extend_from_slice(&id.to_be_bytes());
        q.extend_from_slice(&0x0100u16.to_be_bytes()); // RD
        q.extend_from_slice(&1u16.to_be_bytes());
        q.extend_from_slice(&0u16.to_be_bytes());
        q.extend_from_slice(&0u16.to_be_bytes());
        q.extend_from_slice(&(extra_additional as u16).to_be_bytes());
        for l in name.split('.') {
            q.push(l.len() as u8);
            q.extend_from_slice(l.as_bytes());
        }
        q.push(0);
        q.extend_from_slice(&qtype.to_be_bytes());
        q.extend_from_slice(&CLASS_IN.to_be_bytes());
        if extra_additional {
            // EDNS0 OPT record, as systemd-resolved sends.
            q.extend_from_slice(&[0, 0, 41, 0x04, 0xd0, 0, 0, 0, 0, 0, 0]);
        }
        q
    }

    fn lookup(name: &str) -> Option<Ipv4Addr> {
        (name == "db.acme.example").then_some(Ipv4Addr::new(198, 18, 0, 7))
    }

    fn rcode(r: &[u8]) -> u16 {
        u16::from_be_bytes([r[2], r[3]]) & 0xF
    }
    fn ancount(r: &[u8]) -> u16 {
        u16::from_be_bytes([r[6], r[7]])
    }

    #[test]
    fn a_query_for_an_active_name_gets_its_fake_address() {
        let q = query(0xBEEF, "db.acme.example", TYPE_A, true);
        let r = answer(&q, lookup).unwrap();
        assert_eq!(&r[0..2], &[0xBE, 0xEF], "id echoed");
        let flags = u16::from_be_bytes([r[2], r[3]]);
        assert!(
            flags & 0x8000 != 0 && flags & 0x0400 != 0 && flags & 0x0100 != 0,
            "QR, AA, RD: {flags:#06x}"
        );
        assert_eq!(rcode(&r), 0);
        assert_eq!(ancount(&r), 1);
        assert_eq!(
            u16::from_be_bytes([r[10], r[11]]),
            0,
            "no additional records echoed"
        );
        let tail = &r[r.len() - 16..];
        assert_eq!(&tail[0..2], &[0xC0, 0x0C]);
        assert_eq!(
            u32::from_be_bytes([tail[6], tail[7], tail[8], tail[9]]),
            ANSWER_TTL_SECS
        );
        assert_eq!(&tail[12..16], &[198, 18, 0, 7]);
    }

    #[test]
    fn case_is_ignored() {
        let r = answer(&query(1, "DB.Acme.Example", TYPE_A, false), lookup).unwrap();
        assert_eq!(ancount(&r), 1);
    }

    #[test]
    fn aaaa_and_other_types_get_nodata() {
        for qtype in [
            28u16, /* AAAA */
            16,    /* TXT */
            65,    /* HTTPS */
        ] {
            let r = answer(&query(2, "db.acme.example", qtype, false), lookup).unwrap();
            assert_eq!((rcode(&r), ancount(&r)), (0, 0), "type {qtype}");
        }
    }

    #[test]
    fn other_names_get_nxdomain() {
        for name in ["x.db.acme.example", "other.example", "acme.example"] {
            let r = answer(&query(3, name, TYPE_A, false), lookup).unwrap();
            assert_eq!((rcode(&r), ancount(&r)), (RCODE_NXDOMAIN, 0), "{name}");
        }
    }

    #[test]
    fn malformed_and_non_queries_are_dropped() {
        let good = query(4, "db.acme.example", TYPE_A, false);
        assert!(answer(&good[..11], lookup).is_none(), "short header");
        assert!(
            answer(&good[..good.len() - 2], lookup).is_none(),
            "truncated question"
        );
        let mut resp = good.clone();
        resp[2] |= 0x80;
        assert!(answer(&resp, lookup).is_none(), "a response");
        let mut two = good.clone();
        two[5] = 2;
        assert!(answer(&two, lookup).is_none(), "two questions");
        let mut ptr = good.clone();
        ptr[12] = 0xC0;
        assert!(
            answer(&ptr, lookup).is_none(),
            "compression pointer in qname"
        );
    }

    #[test]
    fn non_standard_opcodes_get_notimp() {
        let mut q = query(5, "db.acme.example", TYPE_A, false);
        q[2] |= 2 << 3; // opcode 2 (STATUS)
        let r = answer(&q, lookup).unwrap();
        assert_eq!((rcode(&r), ancount(&r)), (RCODE_NOTIMP, 0));
    }
}
