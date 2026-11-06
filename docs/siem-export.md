# SIEM export

Zecurity gets audit and access events into a SIEM the way Twingate does, over two channels. Neither channel needs anything on your side that accepts inbound connections.

| Channel | What it carries | Latency | Tamper evidence |
|---|---|---|---|
| 1. Connector access log | One line per access decision, printed by each connector | Real time | None (local, unchained) |
| 2. Amazon S3 export | The workspace's `admin` and `access` audit streams, as gzip NDJSON files in your bucket | About 5 minutes | Hash chain on every event |

The webhook and Splunk HEC sinks (Settings → Audit export) still exist alongside these. Design: `.zecurity-obs/pending/PENDING-11-Audit-Logging-SIEM.md` §9.

---

## 1. Connector access log

### Turning it on

Set `ZECURITY_LOG_ANALYTICS=v1` in the connector's environment, then restart the connector. It is off by default.

- **systemd install:** add `ZECURITY_LOG_ANALYTICS=v1` to `/etc/zecurity/connector.conf`, then `systemctl restart zecurity-connector`.
- **Docker:** add `-e ZECURITY_LOG_ANALYTICS=v1`.

At startup the connector logs `access analytics lines on (ZECURITY_LOG_ANALYTICS=v1)`. Any value other than `v1` is refused with a warning, and the lines stay off.

### Line format

Every access decision prints one line to stdout:

```
ANALYTICS {"schema":"zecurity.connector.analytics.v1","event_type":"access_decision", ...}
```

The line is written whole, so it never interleaves with other log lines. It is written whether or not the connector can reach the controller.

| Field | Meaning |
|---|---|
| `schema` | `zecurity.connector.analytics.v1` |
| `event_type` | `access_decision` |
| `timestamp` | RFC 3339, UTC |
| `decision` | `allow`, `deny` or `error` |
| `error` | The reason for a deny or error, e.g. `no_acl_match` (omitted on allow) |
| `workspace.id`, `connector.id` | Workspace and connector UUIDs |
| `client.spiffe_id` | The device's SPIFFE ID |
| `client.kind`, `client.id` | Parsed from the SPIFFE ID: `client` or `client_device`, and its UUID |
| `resource.id` | Resource UUID (omitted when no resource matched) |
| `resource.destination`, `resource.port`, `resource.protocol` | What the device asked for |
| `route_type` | How the connection was routed (omitted when none) |

**Not in v1:**
- the user's email (the connector doesn't know it);
- connection close, bytes or duration.

The chained copy of the same decisions is the `access` stream in channel 2.

### Wazuh: collect and decode

**1. Decoders.** Add both to `/var/ossec/etc/decoders/local_decoder.xml` on the Wazuh manager. Wazuh only tries the second one once the syslog header has named the program, so journald-collected lines need it.

```xml
<!-- Zecurity connector access analytics (ZECURITY_LOG_ANALYTICS=v1). -->
<decoder name="zecurity-connector-analytics">
  <prematch>^ANALYTICS\s</prematch>
  <plugin_decoder offset="after_prematch">JSON_Decoder</plugin_decoder>
</decoder>

<!-- The same lines from journald / syslog (program name zecurity-connector). -->
<decoder name="zecurity-connector-analytics-syslog">
  <program_name>zecurity-connector</program_name>
  <prematch>^ANALYTICS\s</prematch>
  <plugin_decoder offset="after_prematch">JSON_Decoder</plugin_decoder>
</decoder>
```

The decoded fields appear as `decision`, `error`, `client.kind`, `resource.port` and so on.

**2. Collect.** Configure the Wazuh agent on the connector host.

- **systemd connector**, which logs to journald:

  ```xml
  <localfile>
    <location>journald</location>
    <log_format>journald</log_format>
    <filter field="_SYSTEMD_UNIT">^zecurity-connector.service$</filter>
  </localfile>
  ```

- **Connector writing to a file** (stdout redirected):

  ```xml
  <localfile>
    <location>/var/log/zecurity-connector.log</location>
    <log_format>syslog</log_format>
  </localfile>
  ```

- **Docker connector:** run it with `--log-driver journald --log-opt tag=zecurity-connector`, then collect from journald as above.

**3. Rules.** Write rules on these fields as needed, for example `<decoded_as>zecurity-connector-analytics</decoded_as>` plus `<field name="decision">^deny$</field>`.

**What was tested (2026-11-06):**
- **Live:** Wazuh manager 4.14.10 read the file form and decoded the real connector lines.
- **`wazuh-logtest` only:** the journald (syslog-header) form.
- **Not tested:** the journald `localfile` block and the Docker journald driver.

---

## 2. Amazon S3 export

### What lands in the bucket

Each batch is one object:

```
<prefix>/zecurity/v1/<workspace_id>/<admin|access>/YYYY/MM/DD/HH/<first_seq>-<last_seq>.jsonl.gz
```

- **Format:** gzip, one `zecurity.audit.v1` event per line. These are the same events the webhook sink sends.
- **Hour in the key:** the first event's `recorded_at`, in UTC.
- **When an object is written:** once the oldest waiting event is 5 minutes old, or as soon as 500 events are waiting.
- **Test button:** writes `export.test-<delivery id>.jsonl.gz` at once.
- **Duplicates:** delivery is at-least-once. A retry can repeat events under a different key, so deduplicate on `(workspace_id, stream, seq)`.
- **Object metadata:** `x-amz-meta-zecurity-signature` holds the sink's HMAC of the gzip body (`t=<unix>,v1=<hex HMAC-SHA256(secret, "<t>." + body)>`), using the signing secret shown once when the sink is created.
- **Hash chain:** every event carries `seq`, `prev_hash`, `row_hash` and `canonical`. A verifier checks `row_hash = sha256(prev_hash ‖ base64-decoded canonical)` and that each `prev_hash` equals the previous event's `row_hash`. A gap in `seq` or a broken link shows a deleted or edited row.

### AWS setup

**1. Create the bucket.** It should not be public.

**2. Create an IAM user (or access key) for Zecurity that can only write under the prefix:**

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": "s3:PutObject",
    "Resource": "arn:aws:s3:::<bucket>/<prefix>/*"
  }]
}
```

**3. Add the sink in the console:** Settings → Audit export → Add sink → **Amazon S3**. Fill in:
- bucket and region;
- optional key prefix (`A-Z a-z 0-9 / _ . -`, no leading, trailing or double `/`);
- access key ID and secret access key.

**Endpoint** is only for S3-compatible stores (`https://host[:port]`, path-style).

**How credentials behave:**
- The secret access key is write-only. It is sealed at rest and never shown again.
- Changing the bucket, region, endpoint or access key ID requires entering the secret again.

**Static keys only:** Zecurity uses static access keys. IAM role federation (OIDC) is not supported yet.

### Wazuh: pull with the aws-s3 module ("Custom Logs Buckets")

**1. Create an SQS queue** for this bucket only. Wazuh deletes every message it reads.

**2. Point the bucket at the queue.** Add an S3 event notification (`s3:ObjectCreated:*`, optionally filtered to the prefix) targeting the queue. The queue policy must allow the bucket to send to it.

**3. Give the Wazuh host credentials** in `/root/.aws/credentials` (a profile) that can:
- `sqs:ReceiveMessage`, `sqs:DeleteMessage` and `sqs:GetQueueUrl` on the queue;
- `s3:GetObject` on `<bucket>/<prefix>/*`;
- `sts:GetCallerIdentity`.

**4. Add the module** to `ossec.conf`:

```xml
<wodle name="aws-s3">
  <disabled>no</disabled>
  <interval>5m</interval>
  <run_on_start>yes</run_on_start>
  <subscriber type="buckets">
    <sqs_name>zecurity-audit-events</sqs_name>
    <aws_profile>default</aws_profile>
  </subscriber>
</wodle>
```

**What Wazuh produces:**
- It reads `.jsonl.gz` objects natively and emits one event per line, with location `Wazuh-AWS`.
- The Zecurity fields are nested under `data.aws.*` (`data.aws.action`, `data.aws.stream`, `data.aws.seq`, `data.aws.outcome`, …).
- `data.aws.log_info.log_file` is the object key.

**What was tested (2026-11-06):** Wazuh manager 4.14.10 pulling from an S3 + SQS emulator, with IAM enforcement on and the policies above. The chain was verified from Wazuh's stored copy. Results are in PENDING-11 §9.
