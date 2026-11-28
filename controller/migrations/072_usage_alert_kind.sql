-- Sprint 25 U4 (F26 usage alerts, Decision Record D-61, extends D-50).
--
-- A new built-in tenant rule kind, tenant.usage_over: a tenant's usage of one
-- meter (D-57) so far this UTC month at or above a threshold. It fires once
-- per tenant per month and resolves at the month rollover; it warns only.
--
-- 069_alerts lists the kinds per scope in alert_rules_kind_scope; that file
-- is never edited, so the constraint is replaced here with the new kind
-- added to the tenant kinds. alert_states and alert_events carry no kind
-- list of their own.

ALTER TABLE alert_rules DROP CONSTRAINT alert_rules_kind_scope;
ALTER TABLE alert_rules ADD CONSTRAINT alert_rules_kind_scope CHECK (
    (scope = 'platform' AND kind IN ('relay.offline', 'relay.over_capacity', 'pool.no_eligible_relay',
                                     'controlplane.degraded', 'certificate.expiring'))
 OR (scope = 'tenant'   AND kind IN ('connector.offline', 'shield.offline', 'tenant.no_relay_path',
                                     'tenant.over_quota', 'tenant.usage_over'))
);

-- Seeded like the other tenant rules: disabled until a super-admin turns it
-- on and sets the meter and threshold (500 GB relayed to clients a month as a
-- starting point). Fixed id so docs and tests can name it.
INSERT INTO alert_rules (id, kind, scope, name, enabled, severity, params, for_seconds, renotify_seconds) VALUES
  ('00000000-0000-0000-0002-000000000010', 'tenant.usage_over', 'tenant', 'Tenant usage over threshold', FALSE, 'warning',
   '{"meter": "relay_bytes_to_clients", "threshold": 500000000000}', 0, 0);
