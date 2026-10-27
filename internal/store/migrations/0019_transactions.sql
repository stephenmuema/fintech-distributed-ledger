-- Transactions (T24: FUNDS.TRANSFER), transaction types (FT.TXN.TYPE.CONDITION) and versions
-- (VERSION). Every money movement is one transaction record; each flow record (payment, payout,
-- transfer, refund, deposit, channel item) and every journal entry points back to it.
CREATE TABLE transaction_types (
    id              TEXT PRIMARY KEY,
    version         INTEGER NOT NULL DEFAULT 0,
    code            TEXT NOT NULL UNIQUE,
    description     TEXT NOT NULL,
    flow            TEXT NOT NULL,
    direction       TEXT NOT NULL,
    narrative       TEXT NOT NULL,
    reporting_class TEXT NOT NULL,
    currencies      TEXT NOT NULL,
    min_amount      INTEGER NOT NULL,
    max_amount      INTEGER NOT NULL,
    disabled        INTEGER NOT NULL,
    created_at      DATETIME NOT NULL,
    updated_at      DATETIME NOT NULL
);

CREATE TABLE versions (
    id               TEXT PRIMARY KEY,
    version          INTEGER NOT NULL DEFAULT 0,
    name             TEXT NOT NULL UNIQUE,
    description      TEXT NOT NULL,
    transaction_type TEXT NOT NULL REFERENCES transaction_types (code),
    channels         TEXT NOT NULL,
    authorisers      INTEGER NOT NULL,
    fields           TEXT NOT NULL,
    min_amount       INTEGER NOT NULL,
    max_amount       INTEGER NOT NULL,
    currencies       TEXT NOT NULL,
    input_routines   TEXT NOT NULL,
    auth_routines    TEXT NOT NULL,
    overrides        TEXT NOT NULL,
    reporting_class  TEXT NOT NULL,
    disabled         INTEGER NOT NULL,
    created_at       DATETIME NOT NULL,
    updated_at       DATETIME NOT NULL
);
CREATE INDEX ix_versions_type ON versions (transaction_type);

CREATE TABLE transactions (
    id                      TEXT PRIMARY KEY,
    version                 INTEGER NOT NULL DEFAULT 0,
    reference               TEXT NOT NULL UNIQUE,
    record_status           TEXT NOT NULL,
    status                  TEXT NOT NULL,
    flow_status             TEXT NOT NULL,
    transaction_type        TEXT NOT NULL,
    version_name            TEXT NOT NULL,
    flow                    TEXT NOT NULL,
    direction               TEXT NOT NULL,
    channel                 TEXT NOT NULL,
    reporting_class         TEXT NOT NULL,
    amount                  INTEGER NOT NULL,
    currency                TEXT NOT NULL,
    debit_account           TEXT,
    debit_currency          TEXT,
    debit_amount            INTEGER,
    credit_account          TEXT,
    credit_currency         TEXT,
    credit_amount           INTEGER,
    charge_amount           INTEGER NOT NULL,
    booking_date            TEXT,
    value_date              TEXT,
    narrative               TEXT,
    payment_details         TEXT NOT NULL,
    ordering_customer_id    TEXT,
    ordering_name           TEXT,
    ordering_account        TEXT,
    ordering_phone          TEXT,
    ordering_institution    TEXT,
    beneficiary_customer_id TEXT,
    beneficiary_name        TEXT,
    beneficiary_account     TEXT,
    beneficiary_phone       TEXT,
    beneficiary_institution TEXT,
    rail                    TEXT,
    provider                TEXT,
    provider_reference      TEXT,
    flow_record_type        TEXT,
    flow_record_id          TEXT,
    overrides_accepted      TEXT NOT NULL,
    input                   TEXT NOT NULL,
    inputter                TEXT NOT NULL,
    authoriser              TEXT,
    input_at                DATETIME NOT NULL,
    authorised_at           DATETIME,
    decline_code            TEXT,
    created_at              DATETIME NOT NULL,
    updated_at              DATETIME NOT NULL
);
CREATE INDEX ix_transactions_status ON transactions (record_status, status);
CREATE INDEX ix_transactions_type ON transactions (transaction_type, version_name);
CREATE INDEX ix_transactions_booking ON transactions (booking_date);
CREATE INDEX ix_transactions_debit ON transactions (debit_account, created_at);
CREATE INDEX ix_transactions_credit ON transactions (credit_account, created_at);
CREATE INDEX ix_transactions_provider_ref ON transactions (provider, provider_reference);
CREATE INDEX ix_transactions_flow_record ON transactions (flow_record_id);
CREATE INDEX ix_transactions_created ON transactions (created_at);

ALTER TABLE payments ADD COLUMN transaction_id TEXT;
ALTER TABLE payouts ADD COLUMN transaction_id TEXT;
ALTER TABLE transfers ADD COLUMN transaction_id TEXT;
ALTER TABLE refunds ADD COLUMN transaction_id TEXT;
ALTER TABLE deposits ADD COLUMN transaction_id TEXT;
ALTER TABLE suspense_items ADD COLUMN transaction_id TEXT;
ALTER TABLE journal_entries ADD COLUMN transaction_id TEXT;
CREATE INDEX ix_journal_entries_transaction ON journal_entries (transaction_id);
ALTER TABLE payment_links ADD COLUMN version_name TEXT;
ALTER TABLE standing_orders ADD COLUMN version_name TEXT;
