-- F17: provider_audit_logs is append-only, enforced by the database.
--
-- 026 made the table append-only by convention (the controller only INSERTs).
-- These triggers refuse UPDATE, DELETE and TRUNCATE, so neither a code path
-- nor an ad-hoc statement through the application's connection can rewrite or
-- remove an audit row. TRUNCATE ... CASCADE from a referencing table is
-- refused as well.
--
-- Scope: this stops accidental and application-level changes. The table owner
-- (or a superuser) can still drop or disable the triggers; protecting against
-- that needs a separate database role without ownership, which is out of
-- scope here.

CREATE FUNCTION provider_audit_logs_append_only() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'provider_audit_logs is append-only: % refused', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER provider_audit_logs_no_update_delete
    BEFORE UPDATE OR DELETE ON provider_audit_logs
    FOR EACH ROW EXECUTE FUNCTION provider_audit_logs_append_only();

CREATE TRIGGER provider_audit_logs_no_truncate
    BEFORE TRUNCATE ON provider_audit_logs
    FOR EACH STATEMENT EXECUTE FUNCTION provider_audit_logs_append_only();
