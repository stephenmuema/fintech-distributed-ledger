-- The change log: every update of a historised record, field by field — who changed
-- what, when, and from what to what. Append-only, like the audit trail.
CREATE TABLE record_changes (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_type    TEXT NOT NULL,
    entity_id      TEXT NOT NULL,
    curr_no        INTEGER NOT NULL,
    field          TEXT NOT NULL,
    old_value      TEXT,
    new_value      TEXT,
    changed_by     TEXT NOT NULL,
    changed_at     DATETIME NOT NULL,
    correlation_id TEXT
);
CREATE INDEX ix_record_changes_entity ON record_changes (entity_type, entity_id, id);
CREATE INDEX ix_record_changes_actor ON record_changes (changed_by, changed_at);
CREATE TRIGGER record_changes_no_update BEFORE UPDATE ON record_changes BEGIN SELECT RAISE(ABORT, 'record_changes is append-only'); END;
CREATE TRIGGER record_changes_no_delete BEFORE DELETE ON record_changes BEGIN SELECT RAISE(ABORT, 'record_changes is append-only'); END;

-- Customers' self-service: when contact details last changed (risk), and account closure.
ALTER TABLE customers ADD COLUMN contact_changed_at DATETIME;
ALTER TABLE wallets ADD COLUMN closed_at DATETIME;
ALTER TABLE wallets ADD COLUMN closure_reason TEXT;
