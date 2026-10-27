-- FT · standing orders, executed by Close of Business (T24: STANDING.ORDER).
CREATE TABLE standing_orders (
    id                    TEXT PRIMARY KEY,
    version               INTEGER NOT NULL DEFAULT 0,
    customer_id           TEXT NOT NULL REFERENCES customers (id),
    wallet_id             TEXT NOT NULL REFERENCES wallets (id),
    kind                  TEXT NOT NULL,
    destination_wallet_id TEXT REFERENCES wallets (id),
    rail                  TEXT,
    destination_account   TEXT,
    destination_name      TEXT,
    bank_code             TEXT,
    amount                INTEGER NOT NULL,
    currency              TEXT NOT NULL,
    frequency             TEXT NOT NULL,
    start_date            TEXT NOT NULL,
    end_date              TEXT,
    next_run_date         TEXT NOT NULL,
    narration             TEXT,
    status                TEXT NOT NULL,
    consecutive_failures  INTEGER NOT NULL DEFAULT 0,
    last_run_date         TEXT,
    last_outcome          TEXT,
    created_at            DATETIME NOT NULL,
    updated_at            DATETIME NOT NULL
);
CREATE INDEX ix_standing_orders_due ON standing_orders (status, next_run_date);
CREATE INDEX ix_standing_orders_wallet ON standing_orders (wallet_id);

-- One row per order and due date: a resumed COB can never pay a due date twice.
CREATE TABLE standing_order_executions (
    id                TEXT PRIMARY KEY,
    standing_order_id TEXT NOT NULL REFERENCES standing_orders (id),
    due_date          TEXT NOT NULL,
    status            TEXT NOT NULL,
    transfer_id       TEXT REFERENCES transfers (id),
    payout_id         TEXT REFERENCES payouts (id),
    decline_code      TEXT,
    created_at        DATETIME NOT NULL,
    UNIQUE (standing_order_id, due_date)
);
