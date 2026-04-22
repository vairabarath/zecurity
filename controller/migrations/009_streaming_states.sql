-- Migration 009: Replace polling-gap states with streaming-ready state machine.
--
-- 'managing' and 'removing' existed only to survive the heartbeat polling gap.
-- With bidirectional gRPC streams, instructions are delivered in real time, so
-- these intermediate states are no longer needed.
--
-- 'protecting' (already in schema, previously unused) becomes the single
-- in-flight transitional state for both directions.
-- 'pending_action' column distinguishes apply vs remove while status='protecting'.

ALTER TABLE resources
  ADD COLUMN pending_action TEXT NOT NULL DEFAULT 'apply'
  CHECK (pending_action IN ('apply', 'remove'));

-- Promote any rows stuck in old transitional states before dropping the values.
UPDATE resources SET status = 'protecting', pending_action = 'apply'  WHERE status = 'managing';
UPDATE resources SET status = 'protecting', pending_action = 'remove' WHERE status = 'removing';

ALTER TABLE resources DROP CONSTRAINT resources_status_check;
ALTER TABLE resources ADD CONSTRAINT resources_status_check
  CHECK (status IN ('pending','protecting','protected','unprotected','failed','deleted'));
