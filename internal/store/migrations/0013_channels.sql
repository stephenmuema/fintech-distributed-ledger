-- CH · provider channels (Tieto, Airtel Money, …) and the suspense event outbox that fires
-- provider modules when money lands in their suspense accounts.
CREATE TABLE provider_channels (
    id                  TEXT PRIMARY KEY,
    version             INTEGER NOT NULL DEFAULT 0,
    code                TEXT NOT NULL UNIQUE,
    name                TEXT NOT NULL,
    description         TEXT NOT NULL,
    currencies          TEXT NOT NULL,
    inward              INTEGER NOT NULL,
    outward             INTEGER NOT NULL,
    extension           TEXT NOT NULL,
    inward_type         TEXT NOT NULL,
    outward_type        TEXT NOT NULL,
    settlement_accounts TEXT NOT NULL,
    local_fields        TEXT NOT NULL,
    min_amount          INTEGER NOT NULL,
    max_amount          INTEGER NOT NULL,
    stp                 INTEGER NOT NULL,
    escalate_after_days INTEGER NOT NULL,
    active              INTEGER NOT NULL,
    created_at          DATETIME NOT NULL
);

CREATE TABLE suspense_events (
    id              TEXT PRIMARY KEY,
    version         INTEGER NOT NULL DEFAULT 0,
    item_id         TEXT NOT NULL REFERENCES suspense_items (id),
    type_code       TEXT NOT NULL,
    status          TEXT NOT NULL,
    attempts        INTEGER NOT NULL,
    next_attempt_at DATETIME NOT NULL,
    claimed_at      DATETIME,
    extension       TEXT,
    outcome         TEXT,
    last_error      TEXT,
    created_at      DATETIME NOT NULL,
    delivered_at    DATETIME
);
CREATE INDEX ix_suspense_events_due ON suspense_events (status, next_attempt_at);
CREATE INDEX ix_suspense_events_item ON suspense_events (item_id);
