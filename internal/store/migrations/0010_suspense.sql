-- AC.SUSPENSE · suspense types (parameter records), the open-item sub-ledger and clearings.
CREATE TABLE suspense_types (
    id                  TEXT PRIMARY KEY,
    version             INTEGER NOT NULL DEFAULT 0,
    code                TEXT NOT NULL UNIQUE,
    name                TEXT NOT NULL,
    description         TEXT NOT NULL,
    side                TEXT NOT NULL,
    category            INTEGER NOT NULL UNIQUE,
    gl_code             TEXT NOT NULL,
    currencies          TEXT NOT NULL,
    warn_after_days     INTEGER NOT NULL,
    escalate_after_days INTEGER NOT NULL,
    cob_policy          TEXT NOT NULL,
    write_off_after_days INTEGER NOT NULL,
    write_off_category  INTEGER NOT NULL,
    allowed_targets     TEXT NOT NULL,
    hooks               TEXT NOT NULL,
    auto_clear_at       INTEGER NOT NULL,
    local_fields        TEXT NOT NULL,
    built_in            INTEGER NOT NULL,
    active              INTEGER NOT NULL,
    created_at          DATETIME NOT NULL
);

CREATE TABLE suspense_items (
    id                    TEXT PRIMARY KEY,
    version               INTEGER NOT NULL DEFAULT 0,
    type_code             TEXT NOT NULL,
    account_id            TEXT NOT NULL REFERENCES ledger_accounts (id),
    account_number        TEXT NOT NULL,
    side                  TEXT NOT NULL,
    currency              TEXT NOT NULL,
    amount                INTEGER NOT NULL,
    open_amount           INTEGER NOT NULL,
    status                TEXT NOT NULL,
    reference             TEXT NOT NULL,
    narrative             TEXT NOT NULL,
    source_type           TEXT NOT NULL,
    source_id             TEXT NOT NULL,
    contra_account_number TEXT,
    journal_entry_id      TEXT REFERENCES journal_entries (id),
    opened_on             TEXT NOT NULL,
    opened_at             DATETIME NOT NULL,
    inputter              TEXT NOT NULL,
    authoriser            TEXT,
    assigned_to           TEXT,
    attributes            TEXT NOT NULL,
    classified_at         DATETIME,
    escalated             INTEGER NOT NULL DEFAULT 0,
    closed_on             TEXT
);
CREATE INDEX ix_suspense_items_open ON suspense_items (type_code, status, opened_on);
CREATE INDEX ix_suspense_items_account ON suspense_items (account_id, status);
CREATE INDEX ix_suspense_items_source ON suspense_items (source_type, source_id);

CREATE TABLE suspense_clearings (
    id                    TEXT PRIMARY KEY,
    version               INTEGER NOT NULL DEFAULT 0,
    item_id               TEXT NOT NULL REFERENCES suspense_items (id),
    type_code             TEXT NOT NULL,
    kind                  TEXT NOT NULL,
    target_account_number TEXT NOT NULL,
    currency              TEXT NOT NULL,
    amount                INTEGER NOT NULL,
    narrative             TEXT NOT NULL,
    confidence            INTEGER,
    status                TEXT NOT NULL,
    inputter              TEXT NOT NULL,
    input_at              DATETIME NOT NULL,
    authoriser            TEXT,
    authorised_at         DATETIME,
    rejection_reason      TEXT,
    journal_entry_id      TEXT REFERENCES journal_entries (id)
);
CREATE INDEX ix_suspense_clearings_item ON suspense_clearings (item_id, status);
CREATE INDEX ix_suspense_clearings_status ON suspense_clearings (status, input_at);
