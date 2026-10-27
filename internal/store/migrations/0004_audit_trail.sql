-- Traceability: content hashes on journal entries, correlation ids on audit events, a log of every
-- outbound provider call, and append-only enforcement inside the database itself.

-- SHA-256 over the entry and its lines; the same hash is sealed into the audit chain.
ALTER TABLE journal_entries ADD COLUMN entry_hash TEXT NOT NULL DEFAULT '';

-- Ties an audit event to the API request (X-Correlation-Id) or worker run that caused it.
ALTER TABLE audit_events ADD COLUMN correlation_id TEXT;
CREATE INDEX ix_audit_events_correlation ON audit_events (correlation_id);

-- Every call to a PSP, mobile-money operator or bank: what we asked, what came back, how long it
-- took and how it was classified. Written in its own transaction, so it survives business
-- rollbacks — the record of "did we send it?" must never depend on the outcome.
CREATE TABLE provider_calls (
    id              TEXT PRIMARY KEY,
    provider        TEXT NOT NULL,
    operation       TEXT NOT NULL,
    subject_type    TEXT,
    subject_id      TEXT,
    idempotency_key TEXT,
    attempt         INTEGER NOT NULL,
    outcome         TEXT NOT NULL,
    reference       TEXT,
    error           TEXT,
    request         TEXT,
    response_status INTEGER,
    response        TEXT,
    duration_ms     INTEGER NOT NULL,
    correlation_id  TEXT,
    started_at      DATETIME NOT NULL
);
CREATE INDEX ix_provider_calls_subject ON provider_calls (subject_id);
CREATE INDEX ix_provider_calls_correlation ON provider_calls (correlation_id);
CREATE INDEX ix_provider_calls_provider ON provider_calls (provider, started_at);

-- Append-only: history can be added to, never rewritten. Tampering now needs direct file access
-- *and* dropping these triggers — and the hash chain still exposes it.
CREATE TRIGGER audit_events_no_update BEFORE UPDATE ON audit_events BEGIN SELECT RAISE(ABORT, 'audit_events is append-only'); END;
CREATE TRIGGER audit_events_no_delete BEFORE DELETE ON audit_events BEGIN SELECT RAISE(ABORT, 'audit_events is append-only'); END;
CREATE TRIGGER journal_entries_no_update BEFORE UPDATE ON journal_entries BEGIN SELECT RAISE(ABORT, 'journal_entries is append-only'); END;
CREATE TRIGGER journal_entries_no_delete BEFORE DELETE ON journal_entries BEGIN SELECT RAISE(ABORT, 'journal_entries is append-only'); END;
CREATE TRIGGER journal_lines_no_update BEFORE UPDATE ON journal_lines BEGIN SELECT RAISE(ABORT, 'journal_lines is append-only'); END;
CREATE TRIGGER journal_lines_no_delete BEFORE DELETE ON journal_lines BEGIN SELECT RAISE(ABORT, 'journal_lines is append-only'); END;
CREATE TRIGGER provider_calls_no_update BEFORE UPDATE ON provider_calls BEGIN SELECT RAISE(ABORT, 'provider_calls is append-only'); END;
CREATE TRIGGER provider_calls_no_delete BEFORE DELETE ON provider_calls BEGIN SELECT RAISE(ABORT, 'provider_calls is append-only'); END;
CREATE TRIGGER payment_attempts_no_update BEFORE UPDATE ON payment_attempts BEGIN SELECT RAISE(ABORT, 'payment_attempts is append-only'); END;
CREATE TRIGGER payment_attempts_no_delete BEFORE DELETE ON payment_attempts BEGIN SELECT RAISE(ABORT, 'payment_attempts is append-only'); END;
CREATE TRIGGER inbound_webhook_events_no_update BEFORE UPDATE ON inbound_webhook_events BEGIN SELECT RAISE(ABORT, 'inbound_webhook_events is append-only'); END;
CREATE TRIGGER inbound_webhook_events_no_delete BEFORE DELETE ON inbound_webhook_events BEGIN SELECT RAISE(ABORT, 'inbound_webhook_events is append-only'); END;
CREATE TRIGGER record_history_no_update BEFORE UPDATE ON record_history BEGIN SELECT RAISE(ABORT, 'record_history is append-only'); END;
CREATE TRIGGER record_history_no_delete BEFORE DELETE ON record_history BEGIN SELECT RAISE(ABORT, 'record_history is append-only'); END;
