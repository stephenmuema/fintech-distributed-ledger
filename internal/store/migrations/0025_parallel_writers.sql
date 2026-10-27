-- Parallel writers on the server engines (ADR 0027).
--
-- locks: named row locks a unit of work takes before a check that spans rows ("customer:<id>"
-- before summing a customer's daily usage, "outflow:<date>" before today's outflow). An UPDATE of
-- the row locks it on every engine until commit; the counter only gives the UPDATE something to do.
CREATE TABLE locks (
    name TEXT PRIMARY KEY,
    n    INTEGER NOT NULL
);

-- The audit trail becomes several hash chains, each extended by one unit of work at a time at
-- commit, so writers don't queue on one chain head. Existing events are chain 0.
ALTER TABLE audit_events ADD COLUMN chain INTEGER NOT NULL DEFAULT 0;
CREATE INDEX ix_audit_events_chain ON audit_events (chain, sequence);
CREATE TABLE audit_heads (
    chain     INTEGER PRIMARY KEY,
    last_hash TEXT NOT NULL
);
