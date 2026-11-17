-- 062_tenant_plans.sql — Sprint 23 B1: provider-managed plans (F8; Decision
-- Record D-43, quota keys of D-47).
--
-- A plan is a name, a description and a limit for each of the six quota keys
-- of D-47. A limit is a non-negative integer or an explicit JSON null
-- (unlimited); a plan that leaves out a key is invalid, so a missing key can
-- never be read as "unlimited". No prices, billing or feature entitlements
-- (F34 and a later decision); the plan model can grow a features column
-- without a rebuild.
--
-- Exactly one plan is the default. Every workspace has a plan: a BEFORE
-- INSERT trigger gives a new workspace the CURRENT default unless the insert
-- names one (a fixed column default could not follow a changed default).
-- Plans are configurable data: the seeded Free values are launch defaults.

-- plan_limits_valid: exactly the six D-47 keys, each null or a non-negative
-- integer (JSON number without fraction or exponent, within BIGINT).
CREATE FUNCTION plan_limits_valid(l JSONB) RETURNS BOOLEAN
LANGUAGE sql IMMUTABLE PARALLEL SAFE
SET search_path = pg_catalog
AS $$
    SELECT jsonb_typeof(l) = 'object'
       AND (SELECT count(*) FROM jsonb_object_keys(l)) = 6
       AND l ?& ARRAY['users', 'resources', 'remote_networks', 'connectors', 'shields', 'client_devices']
       AND NOT EXISTS (
           SELECT 1 FROM jsonb_each(l) AS e(key, value)
            WHERE NOT (
                jsonb_typeof(e.value) = 'null'
                OR (jsonb_typeof(e.value) = 'number'
                    AND e.value::text ~ '^[0-9]{1,18}$')
            )
       )
$$;

CREATE TABLE plans (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT        NOT NULL UNIQUE
                            CHECK (name = btrim(name) AND char_length(name) BETWEEN 1 AND 64),
    description TEXT        NOT NULL DEFAULT ''
                            CHECK (char_length(description) <= 500),
    limits      JSONB       NOT NULL CHECK (plan_limits_valid(limits)),
    is_default  BOOLEAN     NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- At most one default row; the application keeps exactly one (set_default
-- clears the old default and sets the new one in one transaction, and the
-- default can't be deleted).
CREATE UNIQUE INDEX plans_one_default ON plans ((true)) WHERE is_default;

-- Seeds (fixed ids so tests and docs can name them).
--   Free   — the default for every new workspace (self-signup and
--            provider-created). Users and resources match Twingate's free
--            Starter tier (5 users, 50 resources, 2026-11-17); the other
--            keys are guardrails.
--   Legacy — every key unlimited; operator-assigned only, never advertised.
INSERT INTO plans (id, name, description, limits, is_default) VALUES
    ('00000000-0000-0000-0001-000000000001', 'Free',
     'Default plan for new workspaces.',
     '{"users": 5, "resources": 50, "remote_networks": 3, "connectors": 10, "shields": 10, "client_devices": 25}',
     true),
    ('00000000-0000-0000-0001-000000000002', 'Legacy',
     'Unlimited compatibility plan; assigned by an operator only.',
     '{"users": null, "resources": null, "remote_networks": null, "connectors": null, "shields": null, "client_devices": null}',
     false);

-- Every workspace has a plan. ON DELETE RESTRICT backs the application rule
-- that an assigned plan can't be deleted.
ALTER TABLE workspaces ADD COLUMN plan_id UUID REFERENCES plans(id) ON DELETE RESTRICT;
-- Existing workspaces keep unlimited quotas (D-43). The pre-production DB is
-- recreated after every schema change, so this normally updates nothing.
UPDATE workspaces SET plan_id = '00000000-0000-0000-0001-000000000002' WHERE plan_id IS NULL;
ALTER TABLE workspaces ALTER COLUMN plan_id SET NOT NULL;
CREATE INDEX workspaces_plan_id ON workspaces (plan_id);

CREATE FUNCTION workspaces_default_plan() RETURNS trigger
LANGUAGE plpgsql
SET search_path = public, pg_catalog
AS $$
BEGIN
    IF NEW.plan_id IS NULL THEN
        SELECT id INTO NEW.plan_id FROM plans WHERE is_default;
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER workspaces_default_plan
    BEFORE INSERT ON workspaces
    FOR EACH ROW EXECUTE FUNCTION workspaces_default_plan();

GRANT SELECT, INSERT, UPDATE, DELETE ON plans TO ztna_app;
