-- Callbacks are triggers, not truth. A transaction status query we initiate is matched by the
-- query id the provider returned to us (never seen by a forger); only its result moves money.
CREATE TABLE status_queries (
    id             TEXT PRIMARY KEY,
    provider       TEXT NOT NULL,
    query_id       TEXT NOT NULL,
    subject_type   TEXT NOT NULL,
    subject_id     TEXT NOT NULL,
    requested_at   DATETIME NOT NULL,
    resolved_at    DATETIME,
    outcome        TEXT,
    UNIQUE (provider, query_id)
);
CREATE INDEX ix_status_queries_subject ON status_queries (subject_id, requested_at);

-- How a deposit was proven real, and the account it will be credited to once validated.
ALTER TABLE deposits ADD COLUMN validation TEXT NOT NULL DEFAULT 'none';
ALTER TABLE deposits ADD COLUMN matched_wallet_id TEXT REFERENCES wallets (id);
ALTER TABLE deposits ADD COLUMN validated_at DATETIME;

-- Set when a payout result callback arrives: the payout is confirmed by a status query, not the callback.
ALTER TABLE payouts ADD COLUMN validation_requested_at DATETIME;
