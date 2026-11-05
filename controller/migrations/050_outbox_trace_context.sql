-- PENDING-10: link an outbox event's dispatch span to the request that
-- published it. Holds the publishing span's W3C traceparent, written only when
-- tracing is enabled and the publishing context carries a valid span; NULL
-- otherwise. tracestate and baggage are deliberately not stored (client-supplied
-- request data; the link needs only the trace and span IDs).
ALTER TABLE outbox_events
    ADD COLUMN traceparent TEXT
        CHECK (traceparent IS NULL
               OR traceparent ~ '^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$');
