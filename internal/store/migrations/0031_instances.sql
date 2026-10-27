-- The instances sharing the database, and their acknowledgements of close of business (ADR 0030).
-- After switching the business date, COB waits until every live instance has acknowledged the
-- run: each first waits for its own units of work that may still book to the closing day.
CREATE TABLE instances (
    id           TEXT PRIMARY KEY,
    started_at   DATETIME NOT NULL,
    heartbeat_at DATETIME NOT NULL
);

CREATE TABLE cob_acks (
    run_id      TEXT NOT NULL,
    instance_id TEXT NOT NULL,
    acked_at    DATETIME NOT NULL,
    PRIMARY KEY (run_id, instance_id)
);
