-- 053_audit_export_s3.sql — PENDING-11 §9 W-3/W-7: Amazon S3 audit export sinks.
--
-- Twingate-style delivery: an 's3' sink writes gzip NDJSON objects to the
-- customer's bucket every 5 minutes, for the SIEM to pull (Wazuh: aws-s3
-- "Custom Logs Buckets"). Its url is derived from its config (bucket, region,
-- optional S3-compatible endpoint); the secret access key is sealed in the
-- auth_header_* slot like the Splunk HEC token. No other column changes.
--
-- 052 is not edited (pre-production rule); this relaxes its kind CHECK.

ALTER TABLE audit_export_sinks DROP CONSTRAINT audit_export_sinks_kind_check;
ALTER TABLE audit_export_sinks ADD CONSTRAINT audit_export_sinks_kind_check
    CHECK (kind IN ('webhook', 'splunk_hec', 's3'));
