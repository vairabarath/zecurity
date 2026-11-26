-- 073_plan_cards.sql — Sprint 26 P1a (F34; Decision Record D-63, D-64, D-65,
-- D-67, D-68, D-69, amendment 2026-11-23, billing).
--
-- Plans (062_, D-43) become priced **plan cards**:
--   * billing_settings: one provider-wide row — billing country and currency
--     (default US / USD, D-64), one tax rate and label (default none, D-69),
--     invoice retention (default 8 years, D-67) and whether issued invoices
--     are emailed to the tenant's admins (default on, D-68).
--   * plan_prices: the price part of a card, one row per plan. No row = the
--     card is not billable and its tenants get no invoice (D-62).
--   * plans.revision + plan_card_revisions: every card edit (and every
--     currency switch) raises the revision and appends the whole card to an
--     append-only history (D-63). The revision in force at the start of a
--     month prices that month.
--   * tenant_billing_plans: the month-start plan (D-65), written by the first
--     plan change in a month, before the change, so a mid-month change bills
--     from the next month.
--   * usage_tenants.plan_id: the plan id kept with the usage snapshot (D-58),
--     so a purged tenant's final month still has its plan.
--
-- Money is whole minor units in BIGINT (D-64); there are no floats or
-- NUMERICs anywhere in billing. Limits rules (062_, 065_) are unchanged.

-- billing_settings ---------------------------------------------------------

CREATE TABLE billing_settings (
    id                      BOOLEAN     PRIMARY KEY DEFAULT true CHECK (id),
    -- ISO 3166-1 alpha-2; choosing a country fills in its currency (P1b).
    country                 TEXT        NOT NULL DEFAULT 'US' CHECK (country ~ '^[A-Z]{2}$'),
    -- ISO 4217; changing it switches every plan card (P1b, D-64).
    currency                TEXT        NOT NULL DEFAULT 'USD' CHECK (currency ~ '^[A-Z]{3}$'),
    -- Basis points: 1800 = 18 %. 0 = no tax ("Tax not included", D-69).
    tax_rate_bp             INTEGER     NOT NULL DEFAULT 0 CHECK (tax_rate_bp BETWEEN 0 AND 10000),
    tax_label               TEXT        NOT NULL DEFAULT '' CHECK (char_length(tax_label) <= 32),
    invoice_retention_years SMALLINT    NOT NULL DEFAULT 8 CHECK (invoice_retention_years BETWEEN 1 AND 30),
    email_tenants           BOOLEAN     NOT NULL DEFAULT true,
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Operator email, or 'system' for the seed.
    updated_by              TEXT        NOT NULL DEFAULT 'system'
);

INSERT INTO billing_settings DEFAULT VALUES;

-- plan_prices ----------------------------------------------------------------

-- plan_price_meters_valid: an object whose keys are a subset of the D-57
-- meters; each value is exactly {included, unit, unit_price_minor}, all
-- non-negative integers (JSON numbers without fraction or exponent, within
-- BIGINT), unit >= 1. {} is valid (base fee only).
CREATE FUNCTION plan_price_meters_valid(m JSONB) RETURNS BOOLEAN
LANGUAGE sql IMMUTABLE PARALLEL SAFE
SET search_path = pg_catalog, pg_temp
AS $$
    SELECT jsonb_typeof(m) = 'object'
       AND NOT EXISTS (
           SELECT 1 FROM jsonb_each(m) AS e(meter, price)
            WHERE NOT (
                e.meter IN ('relay_bytes_to_clients', 'relay_bytes_to_connectors', 'session_minutes',
                            'connector_minutes', 'shield_minutes', 'active_users')
                AND jsonb_typeof(e.price) = 'object'
                AND (SELECT count(*) FROM jsonb_object_keys(e.price)) = 3
                AND e.price ?& ARRAY['included', 'unit', 'unit_price_minor']
                AND jsonb_typeof(e.price -> 'included') = 'number'
                AND (e.price -> 'included')::text ~ '^[0-9]{1,18}$'
                AND jsonb_typeof(e.price -> 'unit') = 'number'
                AND (e.price -> 'unit')::text ~ '^[0-9]{1,18}$'
                AND (e.price ->> 'unit')::bigint >= 1
                AND jsonb_typeof(e.price -> 'unit_price_minor') = 'number'
                AND (e.price -> 'unit_price_minor')::text ~ '^[0-9]{1,18}$'
            )
       )
$$;

CREATE TABLE plan_prices (
    -- Deleting a plan follows B1's rules (an assigned plan can't be deleted);
    -- invoices keep their own snapshot, so the price may go with the plan.
    plan_id        UUID        PRIMARY KEY REFERENCES plans(id) ON DELETE CASCADE,
    -- Always the settings currency (checked by P1b; switched with it, D-64).
    currency       TEXT        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    base_fee_minor BIGINT      NOT NULL CHECK (base_fee_minor >= 0),
    meters         JSONB       NOT NULL CHECK (plan_price_meters_valid(meters)),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by     TEXT        NOT NULL
);

-- Card revisions (D-63) -------------------------------------------------------

ALTER TABLE plans ADD COLUMN revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1);

-- The whole card after each change: {name, description, limits, price}
-- (price null = not billable). Append-only; no foreign key, so a deleted
-- plan's history stays.
CREATE TABLE plan_card_revisions (
    plan_id    UUID        NOT NULL,
    revision   INTEGER     NOT NULL CHECK (revision >= 1),
    card       JSONB       NOT NULL CHECK (jsonb_typeof(card) = 'object'
                                           AND card ?& ARRAY['name', 'description', 'limits', 'price']),
    changed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Operator email, or 'system'.
    changed_by TEXT        NOT NULL,
    reason     TEXT        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 500),
    PRIMARY KEY (plan_id, revision)
);

CREATE FUNCTION plan_card_revisions_append_only() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    RAISE EXCEPTION 'plan_card_revisions is append-only: % refused', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER plan_card_revisions_no_update_delete
    BEFORE UPDATE OR DELETE ON plan_card_revisions
    FOR EACH ROW EXECUTE FUNCTION plan_card_revisions_append_only();

CREATE TRIGGER plan_card_revisions_no_truncate
    BEFORE TRUNCATE ON plan_card_revisions
    FOR EACH STATEMENT EXECUTE FUNCTION plan_card_revisions_append_only();

-- The seeded plans (062_) start at revision 1, unpriced.
INSERT INTO plan_card_revisions (plan_id, revision, card, changed_at, changed_by, reason)
SELECT id, 1,
       jsonb_build_object('name', name, 'description', description, 'limits', limits, 'price', NULL),
       created_at, 'system', 'Initial card'
  FROM plans;

-- Month-start plan (D-65) ------------------------------------------------------

-- Written by the first plan change in a UTC month, in the assign transaction
-- and before the change (ON CONFLICT DO NOTHING), so it always holds the plan
-- the tenant had on the 1st. No foreign keys: a snapshot that outlives a
-- purge until the month's invoice exists. Append-only.
CREATE TABLE tenant_billing_plans (
    tenant_id   UUID        NOT NULL,
    month       DATE        NOT NULL CHECK (EXTRACT(DAY FROM month) = 1),
    plan_id     UUID        NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, month)
);

CREATE FUNCTION tenant_billing_plans_append_only() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, pg_temp
    AS $$
BEGIN
    RAISE EXCEPTION 'tenant_billing_plans is append-only: % refused', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER tenant_billing_plans_no_update
    BEFORE UPDATE ON tenant_billing_plans
    FOR EACH ROW EXECUTE FUNCTION tenant_billing_plans_append_only();

CREATE TRIGGER tenant_billing_plans_no_truncate
    BEFORE TRUNCATE ON tenant_billing_plans
    FOR EACH STATEMENT EXECUTE FUNCTION tenant_billing_plans_append_only();

-- Usage snapshot (D-58) ----------------------------------------------------------

-- Kept current by the U1 collector, frozen at purge (P1b).
ALTER TABLE usage_tenants ADD COLUMN plan_id UUID;

-- Privileges ----------------------------------------------------------------------
-- 053's ALTER DEFAULT PRIVILEGES gives ztna_app SELECT, INSERT, UPDATE and
-- DELETE on new tables; narrow it to what P1b needs.
REVOKE INSERT, DELETE ON billing_settings FROM ztna_app;          -- one row, edited only
REVOKE UPDATE, DELETE ON plan_card_revisions FROM ztna_app;       -- append-only
REVOKE UPDATE, DELETE ON tenant_billing_plans FROM ztna_app;      -- append-only
