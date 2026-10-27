-- Zirnis schema. Money is INTEGER minor units; rates are exact decimals stored as TEXT;
-- business dates are TEXT 'YYYY-MM-DD'; timestamps are DATETIME (UTC).
-- Every versioned table carries a `version` column used for optimistic concurrency.

-- ---------------------------------------------------------------- EB · core
CREATE TABLE system_dates (
    id                 TEXT PRIMARY KEY,
    version            INTEGER NOT NULL DEFAULT 0,
    today              TEXT NOT NULL,
    last_working_day   TEXT NOT NULL,
    next_working_day   TEXT NOT NULL,
    status             TEXT NOT NULL,
    current_cob_run_id TEXT
);

CREATE TABLE holidays (
    country TEXT NOT NULL,
    date    TEXT NOT NULL,
    name    TEXT NOT NULL,
    PRIMARY KEY (country, date)
);

CREATE TABLE number_sequences (
    id         TEXT PRIMARY KEY,
    version    INTEGER NOT NULL DEFAULT 0,
    name       TEXT NOT NULL UNIQUE,
    next_value INTEGER NOT NULL
);

CREATE TABLE cob_runs (
    id                TEXT PRIMARY KEY,
    business_date     TEXT NOT NULL,
    is_month_end      INTEGER NOT NULL,
    status            TEXT NOT NULL,
    started_by        TEXT NOT NULL,
    started_at        DATETIME NOT NULL,
    completed_at      DATETIME,
    new_business_date TEXT,
    report            TEXT
);
CREATE INDEX ix_cob_runs_business_date ON cob_runs (business_date);

CREATE TABLE cob_job_runs (
    id                TEXT PRIMARY KEY,
    run_id            TEXT NOT NULL REFERENCES cob_runs (id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    stage             TEXT NOT NULL,
    job_order         INTEGER NOT NULL,
    status            TEXT NOT NULL,
    attempts          INTEGER NOT NULL,
    records_processed INTEGER NOT NULL,
    summary           TEXT,
    error             TEXT,
    started_at        DATETIME,
    finished_at       DATETIME
);
CREATE INDEX ix_cob_job_runs_run ON cob_job_runs (run_id);

CREATE TABLE charge_types (
    id           TEXT PRIMARY KEY,
    version      INTEGER NOT NULL DEFAULT 0,
    code         TEXT NOT NULL UNIQUE,
    description  TEXT NOT NULL,
    basis_points INTEGER NOT NULL
);

CREATE TABLE interest_conditions (
    id          TEXT PRIMARY KEY,
    version     INTEGER NOT NULL DEFAULT 0,
    category    INTEGER NOT NULL,
    currency    TEXT NOT NULL,
    annual_rate TEXT NOT NULL,
    UNIQUE (category, currency)
);

CREATE TABLE parameter_change_requests (
    id               TEXT PRIMARY KEY,
    version          INTEGER NOT NULL DEFAULT 0,
    type             TEXT NOT NULL,
    key              TEXT NOT NULL,
    payload          TEXT NOT NULL,
    status           TEXT NOT NULL,
    inputter         TEXT NOT NULL,
    input_at         DATETIME NOT NULL,
    authoriser       TEXT,
    authorised_at    DATETIME,
    rejection_reason TEXT
);
CREATE INDEX ix_parameter_changes_pending ON parameter_change_requests (type, key, status);

CREATE TABLE manual_adjustments (
    id               TEXT PRIMARY KEY,
    version          INTEGER NOT NULL DEFAULT 0,
    currency         TEXT NOT NULL,
    narrative        TEXT NOT NULL,
    lines            TEXT NOT NULL,
    status           TEXT NOT NULL,
    inputter         TEXT NOT NULL,
    input_at         DATETIME NOT NULL,
    authoriser       TEXT,
    authorised_at    DATETIME,
    rejection_reason TEXT,
    journal_entry_id TEXT
);
CREATE INDEX ix_manual_adjustments_status ON manual_adjustments (status);

CREATE TABLE record_history (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_type TEXT NOT NULL,
    entity_id   TEXT NOT NULL,
    curr_no     INTEGER NOT NULL,
    snapshot    TEXT NOT NULL,
    changed_by  TEXT NOT NULL,
    changed_at  DATETIME NOT NULL
);
CREATE INDEX ix_record_history_entity ON record_history (entity_type, entity_id, curr_no);

-- Append-only and hash-chained; a fork (two events claiming one predecessor) is impossible.
CREATE TABLE audit_events (
    sequence      INTEGER PRIMARY KEY AUTOINCREMENT,
    actor         TEXT NOT NULL,
    action        TEXT NOT NULL,
    entity_type   TEXT NOT NULL,
    entity_id     TEXT NOT NULL,
    data          TEXT,
    occurred_at   DATETIME NOT NULL,
    previous_hash TEXT NOT NULL UNIQUE,
    hash          TEXT NOT NULL
);
CREATE INDEX ix_audit_events_entity ON audit_events (entity_type, entity_id);

CREATE TABLE outbox_messages (
    id              TEXT PRIMARY KEY,
    type            TEXT NOT NULL,
    customer_id     TEXT NOT NULL,
    payload         TEXT NOT NULL,
    occurred_at     DATETIME NOT NULL,
    status          TEXT NOT NULL,
    attempts        INTEGER NOT NULL,
    next_attempt_at DATETIME,
    delivered_at    DATETIME,
    last_error      TEXT
);
CREATE INDEX ix_outbox_due ON outbox_messages (status, next_attempt_at);

-- The unique (client, key) index is the lock that serialises duplicate requests.
CREATE TABLE idempotency_records (
    id                    TEXT PRIMARY KEY,
    client_id             TEXT NOT NULL,
    key                   TEXT NOT NULL,
    request_hash          TEXT NOT NULL,
    state                 TEXT NOT NULL,
    response_status       INTEGER,
    response_body         TEXT,
    response_content_type TEXT,
    created_at            DATETIME NOT NULL,
    UNIQUE (client_id, key)
);
CREATE INDEX ix_idempotency_created ON idempotency_records (created_at);

CREATE TABLE inbound_webhook_events (
    id          TEXT PRIMARY KEY,
    provider    TEXT NOT NULL,
    event_id    TEXT NOT NULL,
    type        TEXT NOT NULL,
    payload     TEXT NOT NULL,
    outcome     TEXT NOT NULL,
    received_at DATETIME NOT NULL,
    UNIQUE (provider, event_id)
);

-- ---------------------------------------------------------------- ST · customers
CREATE TABLE customers (
    id                     TEXT PRIMARY KEY,
    version                INTEGER NOT NULL DEFAULT 0,
    customer_number        INTEGER NOT NULL UNIQUE,
    mnemonic               TEXT NOT NULL,
    sector                 INTEGER NOT NULL,
    account_officer        INTEGER NOT NULL,
    type                   TEXT NOT NULL,
    legal_name             TEXT NOT NULL,
    email                  TEXT NOT NULL UNIQUE,
    phone                  TEXT,
    country                TEXT NOT NULL,
    kyc_status             TEXT NOT NULL,
    tier                   INTEGER NOT NULL,
    kyc_provider_reference TEXT,
    kyc_rejection_reason   TEXT,
    created_at             DATETIME NOT NULL
);
CREATE INDEX ix_customers_mnemonic ON customers (mnemonic);
CREATE INDEX ix_customers_created ON customers (created_at);

CREATE TABLE webhook_endpoints (
    id          TEXT PRIMARY KEY,
    customer_id TEXT NOT NULL UNIQUE REFERENCES customers (id),
    url         TEXT NOT NULL,
    secret      TEXT NOT NULL,
    created_at  DATETIME NOT NULL
);

-- ---------------------------------------------------------------- EB.ACCOUNTING · ledger
CREATE TABLE ledger_accounts (
    id             TEXT PRIMARY KEY,
    version        INTEGER NOT NULL DEFAULT 0,
    code           TEXT NOT NULL UNIQUE,
    account_number TEXT NOT NULL UNIQUE,
    name           TEXT NOT NULL,
    type           TEXT NOT NULL,
    category       INTEGER NOT NULL,
    gl_code        TEXT NOT NULL,
    currency       TEXT NOT NULL,
    balance        INTEGER NOT NULL,
    allow_negative INTEGER NOT NULL,
    created_at     DATETIME NOT NULL
);
CREATE INDEX ix_ledger_accounts_gl ON ledger_accounts (gl_code);
CREATE INDEX ix_ledger_accounts_category ON ledger_accounts (category, currency);

-- The unique reference is the ledger's idempotency guarantee.
CREATE TABLE journal_entries (
    id                    TEXT PRIMARY KEY,
    reference             TEXT NOT NULL UNIQUE,
    transaction_reference TEXT NOT NULL UNIQUE,
    category              TEXT NOT NULL,
    description           TEXT NOT NULL,
    posted_at             DATETIME NOT NULL,
    booking_date          TEXT NOT NULL,
    value_date            TEXT NOT NULL
);
CREATE INDEX ix_journal_entries_booking ON journal_entries (booking_date, category);
CREATE INDEX ix_journal_entries_posted ON journal_entries (posted_at);

CREATE TABLE journal_lines (
    id               TEXT PRIMARY KEY,
    journal_entry_id TEXT NOT NULL REFERENCES journal_entries (id),
    account_id       TEXT NOT NULL REFERENCES ledger_accounts (id),
    direction        TEXT NOT NULL,
    amount           INTEGER NOT NULL CHECK (amount > 0),
    currency         TEXT NOT NULL,
    balance_after    INTEGER NOT NULL,
    booking_date     TEXT NOT NULL
);
CREATE INDEX ix_journal_lines_account ON journal_lines (account_id, booking_date);
CREATE INDEX ix_journal_lines_entry ON journal_lines (journal_entry_id);
CREATE INDEX ix_journal_lines_booking ON journal_lines (booking_date);

CREATE TABLE daily_balances (
    account_id    TEXT NOT NULL REFERENCES ledger_accounts (id),
    business_date TEXT NOT NULL,
    currency      TEXT NOT NULL,
    opening       INTEGER NOT NULL,
    debits        INTEGER NOT NULL,
    credits       INTEGER NOT NULL,
    closing       INTEGER NOT NULL,
    PRIMARY KEY (account_id, business_date)
);
CREATE INDEX ix_daily_balances_date ON daily_balances (business_date);

-- ---------------------------------------------------------------- AC · accounts
CREATE TABLE wallets (
    id                   TEXT PRIMARY KEY,
    version              INTEGER NOT NULL DEFAULT 0,
    customer_id          TEXT NOT NULL REFERENCES customers (id),
    account_number       TEXT NOT NULL UNIQUE,
    title                TEXT NOT NULL,
    category             INTEGER NOT NULL,
    currency             TEXT NOT NULL,
    available_account_id TEXT NOT NULL REFERENCES ledger_accounts (id),
    held_account_id      TEXT NOT NULL REFERENCES ledger_accounts (id),
    restriction          TEXT NOT NULL,
    restriction_reason   TEXT,
    opening_date         TEXT NOT NULL,
    created_at           DATETIME NOT NULL
);
-- One open account per customer and currency is the application's rule: a closed account
-- (0022) leaves room for a new one.
CREATE INDEX ix_wallets_customer ON wallets (customer_id, currency);
CREATE INDEX ix_wallets_category ON wallets (currency, category);

CREATE TABLE interest_accruals (
    id                      TEXT PRIMARY KEY,
    wallet_id               TEXT NOT NULL REFERENCES wallets (id),
    business_date           TEXT NOT NULL,
    currency                TEXT NOT NULL,
    balance                 INTEGER NOT NULL,
    annual_rate             TEXT NOT NULL,
    days                    INTEGER NOT NULL,
    amount                  INTEGER NOT NULL,
    capitalised             INTEGER NOT NULL,
    capitalisation_entry_id TEXT,
    UNIQUE (wallet_id, business_date)
);
CREATE INDEX ix_interest_accruals_open ON interest_accruals (capitalised, currency);

-- ---------------------------------------------------------------- FT · transfers
CREATE TABLE transfers (
    id                    TEXT PRIMARY KEY,
    source_wallet_id      TEXT NOT NULL REFERENCES wallets (id),
    destination_wallet_id TEXT NOT NULL REFERENCES wallets (id),
    amount                INTEGER NOT NULL,
    currency              TEXT NOT NULL,
    narration             TEXT,
    status                TEXT NOT NULL,
    decline_code          TEXT,
    journal_entry_id      TEXT,
    created_at            DATETIME NOT NULL
);
CREATE INDEX ix_transfers_pair ON transfers (source_wallet_id, destination_wallet_id);

-- ---------------------------------------------------------------- PP · payments
CREATE TABLE payment_links (
    id                   TEXT PRIMARY KEY,
    version              INTEGER NOT NULL DEFAULT 0,
    code                 TEXT NOT NULL UNIQUE,
    merchant_customer_id TEXT NOT NULL REFERENCES customers (id),
    merchant_wallet_id   TEXT NOT NULL REFERENCES wallets (id),
    currency             TEXT NOT NULL,
    amount               INTEGER,
    description          TEXT NOT NULL,
    expires_at           DATETIME,
    max_uses             INTEGER,
    use_count            INTEGER NOT NULL,
    is_active            INTEGER NOT NULL,
    created_at           DATETIME NOT NULL
);
CREATE INDEX ix_payment_links_wallet ON payment_links (merchant_wallet_id);

CREATE TABLE payments (
    id                   TEXT PRIMARY KEY,
    version              INTEGER NOT NULL DEFAULT 0,
    merchant_customer_id TEXT NOT NULL REFERENCES customers (id),
    merchant_wallet_id   TEXT NOT NULL REFERENCES wallets (id),
    amount               INTEGER NOT NULL,
    currency             TEXT NOT NULL,
    method               TEXT NOT NULL,
    payment_source       TEXT NOT NULL,
    description          TEXT,
    payment_link_id      TEXT REFERENCES payment_links (id),
    status               TEXT NOT NULL,
    provider             TEXT,
    provider_reference   TEXT,
    fee                  INTEGER NOT NULL,
    refunded_amount      INTEGER NOT NULL,
    disputed_amount      INTEGER NOT NULL,
    failure_code         TEXT,
    failure_message      TEXT,
    created_at           DATETIME NOT NULL,
    updated_at           DATETIME NOT NULL,
    succeeded_at         DATETIME
);
CREATE INDEX ix_payments_provider_ref ON payments (provider, provider_reference);
CREATE INDEX ix_payments_wallet ON payments (merchant_wallet_id, created_at);
CREATE INDEX ix_payments_status ON payments (status, succeeded_at);

CREATE TABLE payment_attempts (
    id                 TEXT PRIMARY KEY,
    payment_intent_id  TEXT NOT NULL REFERENCES payments (id) ON DELETE CASCADE,
    provider           TEXT NOT NULL,
    outcome            TEXT NOT NULL,
    provider_reference TEXT,
    error_code         TEXT,
    attempted_at       DATETIME NOT NULL
);
CREATE INDEX ix_payment_attempts_payment ON payment_attempts (payment_intent_id);

CREATE TABLE refunds (
    id                 TEXT PRIMARY KEY,
    version            INTEGER NOT NULL DEFAULT 0,
    payment_intent_id  TEXT NOT NULL REFERENCES payments (id),
    amount             INTEGER NOT NULL,
    currency           TEXT NOT NULL,
    provider           TEXT NOT NULL,
    reason             TEXT,
    status             TEXT NOT NULL,
    provider_reference TEXT,
    failure_code       TEXT,
    created_at         DATETIME NOT NULL,
    completed_at       DATETIME
);
CREATE INDEX ix_refunds_payment ON refunds (payment_intent_id);
CREATE INDEX ix_refunds_recon ON refunds (provider, status, completed_at);

CREATE TABLE payouts (
    id                  TEXT PRIMARY KEY,
    version             INTEGER NOT NULL DEFAULT 0,
    customer_id         TEXT NOT NULL REFERENCES customers (id),
    wallet_id           TEXT NOT NULL REFERENCES wallets (id),
    amount              INTEGER NOT NULL,
    fee                 INTEGER NOT NULL,
    currency            TEXT NOT NULL,
    rail                TEXT NOT NULL,
    destination_account TEXT NOT NULL,
    destination_name    TEXT NOT NULL,
    bank_code           TEXT,
    narration           TEXT,
    status              TEXT NOT NULL,
    provider            TEXT,
    provider_reference  TEXT,
    submission_attempts INTEGER NOT NULL,
    next_attempt_at     DATETIME,
    failure_code        TEXT,
    reviewed_by         TEXT,
    created_at          DATETIME NOT NULL,
    updated_at          DATETIME NOT NULL,
    completed_at        DATETIME
);
CREATE INDEX ix_payouts_due ON payouts (status, next_attempt_at);
CREATE INDEX ix_payouts_provider_ref ON payouts (provider, provider_reference);
CREATE INDEX ix_payouts_customer ON payouts (customer_id, status);

CREATE TABLE disputes (
    id                   TEXT PRIMARY KEY,
    version              INTEGER NOT NULL DEFAULT 0,
    payment_intent_id    TEXT NOT NULL REFERENCES payments (id),
    merchant_customer_id TEXT NOT NULL,
    merchant_wallet_id   TEXT NOT NULL,
    amount               INTEGER NOT NULL,
    currency             TEXT NOT NULL,
    reason               TEXT NOT NULL,
    provider_reference   TEXT UNIQUE,
    status               TEXT NOT NULL,
    evidence             TEXT,
    opened_at            DATETIME NOT NULL,
    evidence_due_by      DATETIME NOT NULL,
    resolved_at          DATETIME
);
CREATE INDEX ix_disputes_payment ON disputes (payment_intent_id);

-- ---------------------------------------------------------------- FX · foreign exchange
CREATE TABLE fx_quotes (
    id               TEXT PRIMARY KEY,
    version          INTEGER NOT NULL DEFAULT 0,
    customer_id      TEXT NOT NULL REFERENCES customers (id),
    source_currency  TEXT NOT NULL,
    target_currency  TEXT NOT NULL,
    source_amount    INTEGER NOT NULL,
    fee              INTEGER NOT NULL,
    mid_rate         TEXT NOT NULL,
    spread_bps       INTEGER NOT NULL,
    target_amount    INTEGER NOT NULL,
    status           TEXT NOT NULL,
    created_at       DATETIME NOT NULL,
    expires_at       DATETIME NOT NULL,
    executed_at      DATETIME,
    source_wallet_id TEXT,
    target_wallet_id TEXT
);

-- ---------------------------------------------------------------- CM · cards
CREATE TABLE cards (
    id                    TEXT PRIMARY KEY,
    version               INTEGER NOT NULL DEFAULT 0,
    customer_id           TEXT NOT NULL REFERENCES customers (id),
    wallet_id             TEXT NOT NULL REFERENCES wallets (id),
    currency              TEXT NOT NULL,
    token                 TEXT NOT NULL UNIQUE,
    last4                 TEXT NOT NULL,
    brand                 TEXT NOT NULL,
    expiry_month          INTEGER NOT NULL,
    expiry_year           INTEGER NOT NULL,
    per_transaction_limit INTEGER,
    status                TEXT NOT NULL,
    created_at            DATETIME NOT NULL
);
CREATE INDEX ix_cards_wallet ON cards (wallet_id);

-- Network retries are idempotent on the unique network reference.
CREATE TABLE card_authorizations (
    id                     TEXT PRIMARY KEY,
    version                INTEGER NOT NULL DEFAULT 0,
    card_id                TEXT NOT NULL REFERENCES cards (id),
    wallet_id              TEXT NOT NULL REFERENCES wallets (id),
    network_reference      TEXT NOT NULL UNIQUE,
    amount                 INTEGER NOT NULL,
    currency               TEXT NOT NULL,
    merchant_name          TEXT NOT NULL,
    merchant_category_code TEXT NOT NULL,
    status                 TEXT NOT NULL,
    decline_code           TEXT,
    captured_amount        INTEGER NOT NULL,
    created_at             DATETIME NOT NULL,
    updated_at             DATETIME NOT NULL
);
CREATE INDEX ix_card_authorizations_status ON card_authorizations (status, created_at);
CREATE INDEX ix_card_authorizations_card ON card_authorizations (card_id);

-- ---------------------------------------------------------------- RK · risk
CREATE TABLE risk_assessments (
    id          TEXT PRIMARY KEY,
    customer_id TEXT NOT NULL,
    operation   TEXT NOT NULL,
    subject_id  TEXT,
    amount      INTEGER NOT NULL,
    currency    TEXT NOT NULL,
    score       INTEGER NOT NULL,
    decision    TEXT NOT NULL,
    reasons     TEXT NOT NULL,
    created_at  DATETIME NOT NULL
);
CREATE INDEX ix_risk_assessments_customer ON risk_assessments (customer_id, created_at);
CREATE INDEX ix_risk_assessments_subject ON risk_assessments (subject_id);

CREATE TABLE blocked_counterparties (
    id         TEXT PRIMARY KEY,
    value      TEXT NOT NULL UNIQUE,
    reason     TEXT NOT NULL,
    created_by TEXT NOT NULL,
    created_at DATETIME NOT NULL
);

-- ---------------------------------------------------------------- RC · reconciliation
CREATE TABLE reconciliation_runs (
    id                  TEXT PRIMARY KEY,
    provider            TEXT NOT NULL,
    report_date         TEXT NOT NULL,
    created_by          TEXT NOT NULL,
    created_at          DATETIME NOT NULL,
    settlement_entry_id TEXT,
    settled_at          DATETIME,
    UNIQUE (provider, report_date)
);

CREATE TABLE reconciliation_items (
    id                 TEXT PRIMARY KEY,
    run_id             TEXT NOT NULL REFERENCES reconciliation_runs (id) ON DELETE CASCADE,
    kind               TEXT NOT NULL,
    provider_reference TEXT NOT NULL,
    currency           TEXT NOT NULL,
    report_amount      INTEGER,
    report_fee         INTEGER,
    ledger_amount      INTEGER,
    outcome            TEXT NOT NULL,
    resolved           INTEGER NOT NULL,
    resolution_note    TEXT,
    resolved_by        TEXT,
    resolved_at        DATETIME
);
CREATE INDEX ix_reconciliation_items_run ON reconciliation_items (run_id);
