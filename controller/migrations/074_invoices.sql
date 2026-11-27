-- 074_invoices.sql — Sprint 26 P1a (F34; Decision Record D-66, D-67, D-68,
-- D-69, amendment 2026-11-23, billing).
--
-- One invoice per tenant and closed UTC month, generated as a draft by the
-- month-close job (P2), then issued, then paid or void (D-66):
--   * an invoice is a snapshot: tenant name and slug, plan name and card
--     revision, currency, tax rate and label, and each line's included
--     quantity, unit and unit price. Later card edits, currency or tax
--     changes and renames never change it;
--   * once issued, triggers refuse every change except issued -> paid (the
--     paid_* fields) and issued -> void (the void fields). Lines can't change;
--   * numbers come from a counter row locked in the issue transaction
--     (invoice_number_counters), gap-free, never a SEQUENCE;
--   * one non-void invoice per (tenant, month): a unique index;
--   * no foreign key to workspaces: invoices are financial records that
--     outlive the S4 purge (D-67, amends D-46). Only prune_invoices(), after
--     the retention period of billing_settings, deletes an issued invoice.
--   * invoice_emails is the D-68 delivery queue (P3 owns the sender).
--
-- Money is whole minor units in BIGINT (D-64); quantities are counts (D-59).

CREATE TABLE invoices (
    id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- INV-<year>-<6 digits>, assigned at issue; null while draft.
    number            TEXT        UNIQUE CHECK (number ~ '^INV-[0-9]{4}-[0-9]{6,}$'),

    -- Tenant snapshot (no foreign key, D-67).
    tenant_id         UUID        NOT NULL,
    tenant_name       TEXT        NOT NULL,
    tenant_slug       TEXT        NOT NULL,
    month             DATE        NOT NULL CHECK (EXTRACT(DAY FROM month) = 1),
    status            TEXT        NOT NULL DEFAULT 'draft'
                                  CHECK (status IN ('draft', 'issued', 'paid', 'void')),

    -- Price and tax snapshot.
    currency          TEXT        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    plan_id           UUID        NOT NULL,
    plan_name         TEXT        NOT NULL,
    plan_revision     INTEGER     NOT NULL CHECK (plan_revision >= 1),
    subtotal_minor    BIGINT      NOT NULL CHECK (subtotal_minor >= 0),
    tax_rate_bp       INTEGER     NOT NULL CHECK (tax_rate_bp BETWEEN 0 AND 10000),
    tax_label         TEXT        NOT NULL DEFAULT '' CHECK (char_length(tax_label) <= 32),
    tax_minor         BIGINT      NOT NULL CHECK (tax_minor >= 0),
    total_minor       BIGINT      NOT NULL CHECK (total_minor >= 0),

    -- Completeness (D-59): what the collector counted; the hours it missed are
    -- billed as counted and stated on the invoice.
    collected_minutes INTEGER     NOT NULL CHECK (collected_minutes >= 0),
    month_minutes     INTEGER     NOT NULL CHECK (month_minutes > 0),
    incomplete_hours  INTEGER     NOT NULL CHECK (incomplete_hours >= 0),

    -- Lifecycle. *_by is the operator email (a snapshot), or 'system'.
    generated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    issued_at         TIMESTAMPTZ,
    issued_by         TEXT,
    paid_at           DATE,
    paid_reference    TEXT        CHECK (char_length(paid_reference) <= 200),
    paid_by           TEXT,
    voided_at         TIMESTAMPTZ,
    voided_by         TEXT,
    void_reason       TEXT        CHECK (char_length(void_reason) <= 500),

    CONSTRAINT invoices_total CHECK (total_minor = subtotal_minor + tax_minor),
    CONSTRAINT invoices_minutes CHECK (collected_minutes <= month_minutes),
    -- The fields each status has, and only those.
    CONSTRAINT invoices_status_fields CHECK (
        CASE status
        WHEN 'draft' THEN
            number IS NULL AND issued_at IS NULL AND issued_by IS NULL
            AND paid_at IS NULL AND paid_reference IS NULL AND paid_by IS NULL
            AND voided_at IS NULL AND voided_by IS NULL AND void_reason IS NULL
        WHEN 'issued' THEN
            number IS NOT NULL AND issued_at IS NOT NULL AND issued_by IS NOT NULL
            AND paid_at IS NULL AND paid_reference IS NULL AND paid_by IS NULL
            AND voided_at IS NULL AND voided_by IS NULL AND void_reason IS NULL
        WHEN 'paid' THEN
            number IS NOT NULL AND issued_at IS NOT NULL AND issued_by IS NOT NULL
            AND paid_at IS NOT NULL AND paid_by IS NOT NULL
            AND voided_at IS NULL AND voided_by IS NULL AND void_reason IS NULL
        WHEN 'void' THEN
            number IS NOT NULL AND issued_at IS NOT NULL AND issued_by IS NOT NULL
            AND paid_at IS NULL AND paid_reference IS NULL AND paid_by IS NULL
            AND voided_at IS NOT NULL AND voided_by IS NOT NULL
            AND void_reason IS NOT NULL AND btrim(void_reason) <> ''
        END
    )
);

-- One non-void invoice per tenant and month (D-66). Regenerating replaces a
-- draft in place; a correction is a void plus a new invoice.
CREATE UNIQUE INDEX invoices_one_per_month ON invoices (tenant_id, month) WHERE status <> 'void';
CREATE INDEX invoices_month ON invoices (month, status);

CREATE TABLE invoice_lines (
    invoice_id       UUID     NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    position         SMALLINT NOT NULL CHECK (position >= 0),
    kind             TEXT     NOT NULL CHECK (kind IN ('base', 'meter')),
    meter            TEXT     CHECK (meter IN ('relay_bytes_to_clients', 'relay_bytes_to_connectors', 'session_minutes',
                                               'connector_minutes', 'shield_minutes', 'active_users')),
    quantity         BIGINT   CHECK (quantity >= 0),
    included         BIGINT   CHECK (included >= 0),
    unit             BIGINT   CHECK (unit >= 1),
    billable_units   BIGINT   CHECK (billable_units >= 0),
    unit_price_minor BIGINT   CHECK (unit_price_minor >= 0),
    amount_minor     BIGINT   NOT NULL CHECK (amount_minor >= 0),
    PRIMARY KEY (invoice_id, position),
    -- A base line is the monthly fee alone. A meter line bills whole units
    -- above the included quantity, rounded up (D-64); the DB re-checks the
    -- arithmetic (bigint overflow raises, it never wraps).
    CONSTRAINT invoice_lines_kind_fields CHECK (
        CASE kind
        WHEN 'base' THEN
            meter IS NULL AND quantity IS NULL AND included IS NULL AND unit IS NULL
            AND billable_units IS NULL AND unit_price_minor IS NULL
        WHEN 'meter' THEN
            meter IS NOT NULL AND quantity IS NOT NULL AND included IS NOT NULL AND unit IS NOT NULL
            AND billable_units IS NOT NULL AND unit_price_minor IS NOT NULL
            AND billable_units = CASE WHEN quantity > included
                                      THEN (quantity - included) / unit
                                           + CASE WHEN (quantity - included) % unit > 0 THEN 1 ELSE 0 END
                                      ELSE 0 END
            AND amount_minor = billable_units * unit_price_minor
        END
    )
);

-- The gap-free counter (D-66): the issue transaction locks the year's row
-- (INSERT ... ON CONFLICT DO UPDATE ... RETURNING), so a rolled-back issue
-- releases its number and no two issues share one.
CREATE TABLE invoice_number_counters (
    year INTEGER PRIMARY KEY CHECK (year BETWEEN 2000 AND 9999),
    last BIGINT  NOT NULL CHECK (last >= 0)
);

-- The tenant email queue (D-68). seq is 1 for issued and voided, n for the
-- n-th resend. 'sending' = claimed by the worker; a row a restart leaves in
-- 'sending' becomes 'failed' ("delivery unknown") and is never re-sent
-- automatically. Recipient addresses are never stored (only the count), and
-- last_error is sanitized by the sender.
CREATE TABLE invoice_emails (
    invoice_id       UUID        NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    kind             TEXT        NOT NULL CHECK (kind IN ('issued', 'voided', 'resend')),
    seq              INTEGER     NOT NULL CHECK (seq >= 1),
    status           TEXT        NOT NULL DEFAULT 'pending'
                                 CHECK (status IN ('pending', 'sending', 'sent', 'failed', 'not_delivered')),
    recipients_count INTEGER     NOT NULL DEFAULT 0 CHECK (recipients_count >= 0),
    attempts         INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_error       TEXT        CHECK (char_length(last_error) <= 1000),
    sent_at          TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (invoice_id, kind, seq),
    CONSTRAINT invoice_emails_once CHECK (kind = 'resend' OR seq = 1),
    CONSTRAINT invoice_emails_sent CHECK ((status = 'sent') = (sent_at IS NOT NULL))
);

CREATE INDEX invoice_emails_due ON invoice_emails (status, next_attempt_at);

-- Immutability (D-66) ----------------------------------------------------------

-- True only inside prune_invoices(): its transaction-local flag is set AND the
-- statement runs as the function's owner (SECURITY DEFINER). The runtime role
-- can set the flag itself but can't become the owner.
--
-- Every function here lists pg_temp LAST in its search_path and names its
-- tables with their schema: otherwise pg_temp is searched first, and a
-- temporary table called invoices or billing_settings could fool a guard.
CREATE FUNCTION invoices_prune_active() RETURNS BOOLEAN
LANGUAGE plpgsql STABLE
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RETURN coalesce(current_setting('zecurity.invoice_prune', true), '') = 'on'
       AND current_user = (SELECT pg_catalog.pg_get_userbyid(p.proowner) FROM pg_catalog.pg_proc p
                            WHERE p.oid = 'public.prune_invoices(date)'::regprocedure);
END;
$$;

CREATE FUNCTION invoices_guard_update() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    paid_fields CONSTANT TEXT[] := ARRAY['status', 'paid_at', 'paid_reference', 'paid_by'];
    void_fields CONSTANT TEXT[] := ARRAY['status', 'voided_at', 'voided_by', 'void_reason'];
BEGIN
    IF OLD.status = 'draft' THEN
        -- A draft may change freely and may be issued. It is deleted, never
        -- voided, and can't be paid before it is issued.
        IF NEW.status IN ('draft', 'issued') THEN
            RETURN NEW;
        END IF;
    ELSIF to_jsonb(NEW) = to_jsonb(OLD) THEN
        RETURN NEW;  -- no change
    ELSIF OLD.status = 'issued' AND NEW.status = 'paid'
          AND to_jsonb(NEW) - paid_fields = to_jsonb(OLD) - paid_fields THEN
        RETURN NEW;
    ELSIF OLD.status = 'issued' AND NEW.status = 'void'
          AND to_jsonb(NEW) - void_fields = to_jsonb(OLD) - void_fields THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'invoice % is %: change refused (% -> %)', OLD.id, OLD.status, OLD.status, NEW.status
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER invoices_immutable_update
    BEFORE UPDATE ON invoices
    FOR EACH ROW EXECUTE FUNCTION invoices_guard_update();

CREATE FUNCTION invoices_guard_delete() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF OLD.status = 'draft' OR public.invoices_prune_active() THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'invoice % is %: only prune_invoices() deletes it', OLD.id, OLD.status
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER invoices_immutable_delete
    BEFORE DELETE ON invoices
    FOR EACH ROW EXECUTE FUNCTION invoices_guard_delete();

CREATE FUNCTION invoice_lines_guard() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    parent_status TEXT;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'invoice_lines can''t be updated: delete and insert while the invoice is a draft'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    SELECT i.status INTO parent_status FROM public.invoices i
     WHERE i.id = CASE WHEN TG_OP = 'DELETE' THEN OLD.invoice_id ELSE NEW.invoice_id END;
    -- No parent: an INSERT is refused; a DELETE is the cascade of a draft's
    -- delete (the parent's own trigger allowed it).
    IF parent_status IS NULL AND TG_OP = 'INSERT' THEN
        RAISE EXCEPTION 'invoice % not found', NEW.invoice_id
            USING ERRCODE = 'foreign_key_violation';
    END IF;
    IF parent_status IS NULL OR parent_status = 'draft' OR public.invoices_prune_active() THEN
        RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
    END IF;
    RAISE EXCEPTION 'invoice is %: its lines can''t change', parent_status
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER invoice_lines_immutable
    BEFORE INSERT OR UPDATE OR DELETE ON invoice_lines
    FOR EACH ROW EXECUTE FUNCTION invoice_lines_guard();

CREATE FUNCTION invoices_no_truncate() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION '% can''t be truncated', TG_TABLE_NAME
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER invoices_no_truncate
    BEFORE TRUNCATE ON invoices
    FOR EACH STATEMENT EXECUTE FUNCTION invoices_no_truncate();

CREATE TRIGGER invoice_lines_no_truncate
    BEFORE TRUNCATE ON invoice_lines
    FOR EACH STATEMENT EXECUTE FUNCTION invoices_no_truncate();

-- Retention (D-67) ------------------------------------------------------------

-- prune_invoices(cutoff): deletes every invoice with month < cutoff (with its
-- lines and emails) and returns how many. The only path that deletes an
-- issued, paid or void invoice. It refuses a cutoff inside the retention
-- period of billing_settings (first day of the current UTC month minus
-- invoice_retention_years), so a wrong caller can't shorten retention.
CREATE FUNCTION prune_invoices(p_cutoff DATE) RETURNS BIGINT
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    years  SMALLINT;
    oldest DATE;
    n      BIGINT;
BEGIN
    SELECT invoice_retention_years INTO years FROM public.billing_settings;
    oldest := (date_trunc('month', now() AT TIME ZONE 'UTC') - make_interval(years => years))::date;
    IF p_cutoff IS NULL OR p_cutoff > oldest THEN
        RAISE EXCEPTION 'prune_invoices: cutoff % is inside the % year retention (oldest allowed %)',
            p_cutoff, years, oldest
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    PERFORM set_config('zecurity.invoice_prune', 'on', true);
    DELETE FROM public.invoices WHERE month < p_cutoff;
    GET DIAGNOSTICS n = ROW_COUNT;
    PERFORM set_config('zecurity.invoice_prune', '', true);
    RETURN n;
END;
$$;

-- Privileges ------------------------------------------------------------------
-- 053's default privileges give ztna_app SELECT, INSERT, UPDATE and DELETE on
-- the four tables (the triggers guard them). The prune is the owner's.
REVOKE ALL ON FUNCTION prune_invoices(DATE) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION prune_invoices(DATE) TO ztna_app;
