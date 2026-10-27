-- NV · correspondents, nostro accounts, their statements and statement-to-ledger matching.
CREATE TABLE correspondents (
    id          TEXT PRIMARY KEY,
    version     INTEGER NOT NULL DEFAULT 0,
    code        TEXT NOT NULL UNIQUE,
    name        TEXT NOT NULL,
    kind        TEXT NOT NULL,
    bic         TEXT NOT NULL,
    bank_code   TEXT NOT NULL,
    country     TEXT NOT NULL,
    provider    TEXT NOT NULL,
    customer_id TEXT REFERENCES customers (id),
    active      INTEGER NOT NULL,
    created_at  DATETIME NOT NULL
);

CREATE TABLE nostro_accounts (
    id                    TEXT PRIMARY KEY,
    version               INTEGER NOT NULL DEFAULT 0,
    code                  TEXT NOT NULL UNIQUE,
    correspondent_code    TEXT NOT NULL REFERENCES correspondents (code),
    currency              TEXT NOT NULL,
    external_account      TEXT NOT NULL,
    ledger_account_id     TEXT NOT NULL UNIQUE REFERENCES ledger_accounts (id),
    ledger_account_number TEXT NOT NULL,
    statement_source      TEXT NOT NULL,
    value_date_window     INTEGER NOT NULL,
    park_after_days       INTEGER NOT NULL,
    credit_suspense_type  TEXT NOT NULL,
    debit_suspense_type   TEXT NOT NULL,
    hooks                 TEXT NOT NULL,
    proof_policy          TEXT NOT NULL,
    reconciled_from       TEXT,
    opening_break         INTEGER NOT NULL DEFAULT 0,
    active                INTEGER NOT NULL,
    created_at            DATETIME NOT NULL
);

CREATE TABLE nostro_statements (
    id               TEXT PRIMARY KEY,
    version          INTEGER NOT NULL DEFAULT 0,
    nostro_id        TEXT NOT NULL REFERENCES nostro_accounts (id),
    nostro_code      TEXT NOT NULL,
    format           TEXT NOT NULL,
    statement_number TEXT NOT NULL,
    currency         TEXT NOT NULL,
    opening_balance  INTEGER,
    closing_balance  INTEGER,
    from_date        TEXT NOT NULL,
    to_date          TEXT NOT NULL,
    line_count       INTEGER NOT NULL,
    content_hash     TEXT NOT NULL,
    status           TEXT NOT NULL,
    proof            TEXT,
    received_by      TEXT NOT NULL,
    received_at      DATETIME NOT NULL,
    UNIQUE (nostro_id, content_hash)
);
CREATE INDEX ix_nostro_statements_nostro ON nostro_statements (nostro_id, to_date);

CREATE TABLE nostro_matches (
    id         TEXT PRIMARY KEY,
    nostro_id  TEXT NOT NULL REFERENCES nostro_accounts (id),
    method     TEXT NOT NULL,
    matched_by TEXT NOT NULL,
    confidence INTEGER,
    created_at DATETIME NOT NULL
);

CREATE TABLE nostro_statement_lines (
    id                 TEXT PRIMARY KEY,
    version            INTEGER NOT NULL DEFAULT 0,
    statement_id       TEXT NOT NULL REFERENCES nostro_statements (id),
    nostro_id          TEXT NOT NULL REFERENCES nostro_accounts (id),
    line_no            INTEGER NOT NULL,
    value_date         TEXT NOT NULL,
    entry_date         TEXT,
    direction          TEXT NOT NULL,
    amount             INTEGER NOT NULL,
    bank_reference     TEXT NOT NULL,
    customer_reference TEXT NOT NULL,
    narrative          TEXT NOT NULL,
    status             TEXT NOT NULL,
    match_id           TEXT REFERENCES nostro_matches (id),
    suspense_item_id   TEXT REFERENCES suspense_items (id)
);
CREATE INDEX ix_nostro_lines_open ON nostro_statement_lines (nostro_id, status, value_date);
CREATE INDEX ix_nostro_lines_statement ON nostro_statement_lines (statement_id, line_no);

-- A ledger line is explained by at most one match.
CREATE TABLE nostro_ledger_links (
    journal_line_id TEXT PRIMARY KEY REFERENCES journal_lines (id),
    match_id        TEXT NOT NULL REFERENCES nostro_matches (id),
    nostro_id       TEXT NOT NULL REFERENCES nostro_accounts (id)
);
CREATE INDEX ix_nostro_links_match ON nostro_ledger_links (match_id);
