-- Production safety: the manual KYC review queue, treasury-maintained FX rates (T24 CURRENCY) and
-- settlement holds on collections (merchants are paid once the acquirer has paid us).
CREATE TABLE kyc_submissions (
    id                  TEXT PRIMARY KEY,
    version             INTEGER NOT NULL DEFAULT 0,
    customer_id         TEXT NOT NULL REFERENCES customers (id),
    document_type       TEXT NOT NULL,
    document_number     TEXT NOT NULL,
    date_of_birth       TEXT,
    registration_number TEXT,
    directors           TEXT NOT NULL,
    submitted_at        DATETIME NOT NULL,
    decided_at          DATETIME,
    decision            TEXT
);
CREATE INDEX ix_kyc_submissions_customer ON kyc_submissions (customer_id);

CREATE TABLE currency_rates (
    id       TEXT PRIMARY KEY,
    version  INTEGER NOT NULL DEFAULT 0,
    currency TEXT NOT NULL UNIQUE,
    per_usd  TEXT NOT NULL,
    as_of    DATETIME NOT NULL
);

ALTER TABLE payments ADD COLUMN held_amount INTEGER NOT NULL DEFAULT 0;
ALTER TABLE payments ADD COLUMN release_on TEXT;
ALTER TABLE provider_channels ADD COLUMN settlement_hold_days INTEGER NOT NULL DEFAULT 0;
