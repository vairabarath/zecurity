-- Relay-side residency enforcement (Decision Record D-40; relay-pools PR 3).
--
-- Each relay receives its pool's workspace policy in every heartbeat
-- response: whether the pool enforces residency, the workspaces allowed on it,
-- a version and a lease. The version comes from one sequence shared by every
-- pool, so it only ever increases for a relay, also when the relay moves to
-- another pool (the destination is bumped). Any change to a pool's authorized
-- workspace set, or to its residency, takes the next value in the same
-- transaction.
CREATE SEQUENCE relay_policy_version_seq;

ALTER TABLE relay_pools
    ADD COLUMN policy_version BIGINT NOT NULL DEFAULT nextval('relay_policy_version_seq');

-- What each relay last reported: it can enforce (policy_supported) and the
-- version it applied, and when. A residency pool is shown as enforced only
-- while every active relay in it supports the policy and has applied the
-- pool's current version.
ALTER TABLE relays
    ADD COLUMN policy_supported     BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN policy_version_acked BIGINT,
    ADD COLUMN policy_acked_at      TIMESTAMPTZ;

-- 053 granted the runtime role only the objects that existed then.
GRANT USAGE, SELECT, UPDATE ON SEQUENCE relay_policy_version_seq TO ztna_app;
