-- Inbound deposits (M-Pesa paybill, bank transfers into partner-bank accounts) identified by the
-- payer's reference. The unique (provider, provider_reference) makes every notification idempotent.
CREATE TABLE deposits (
    id                 TEXT PRIMARY KEY,
    version            INTEGER NOT NULL DEFAULT 0,
    provider           TEXT NOT NULL,
    provider_reference TEXT NOT NULL,
    account_reference  TEXT NOT NULL,
    amount             INTEGER NOT NULL CHECK (amount > 0),
    currency           TEXT NOT NULL,
    payer_identifier   TEXT,
    payer_name         TEXT,
    status             TEXT NOT NULL,
    wallet_id          TEXT REFERENCES wallets (id),
    proposed_wallet_id TEXT REFERENCES wallets (id),
    proposed_by        TEXT,
    applied_by         TEXT,
    received_at        DATETIME NOT NULL,
    applied_at         DATETIME,
    UNIQUE (provider, provider_reference)
);
CREATE INDEX ix_deposits_status ON deposits (status, received_at);
CREATE INDEX ix_deposits_wallet ON deposits (wallet_id);
