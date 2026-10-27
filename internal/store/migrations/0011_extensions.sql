-- EB.API · remote extensions (hook endpoints, maker-checker) and the call log of every extension call.
CREATE TABLE hook_endpoints (
    id             TEXT PRIMARY KEY,
    version        INTEGER NOT NULL DEFAULT 0,
    name           TEXT NOT NULL UNIQUE,
    url            TEXT NOT NULL,
    hooks          TEXT NOT NULL,
    secret_env     TEXT NOT NULL,
    timeout_ms     INTEGER NOT NULL,
    failure_policy TEXT NOT NULL,
    enabled        INTEGER NOT NULL,
    created_at     DATETIME NOT NULL
);

CREATE TABLE hook_calls (
    id             TEXT PRIMARY KEY,
    extension      TEXT NOT NULL,
    hook           TEXT NOT NULL,
    remote         INTEGER NOT NULL,
    subject        TEXT NOT NULL,
    delivery_id    TEXT NOT NULL,
    outcome        TEXT NOT NULL,
    latency_ms     INTEGER NOT NULL,
    error          TEXT,
    correlation_id TEXT,
    called_at      DATETIME NOT NULL
);
CREATE INDEX ix_hook_calls_extension ON hook_calls (extension, called_at);
CREATE INDEX ix_hook_calls_subject ON hook_calls (subject, called_at);
