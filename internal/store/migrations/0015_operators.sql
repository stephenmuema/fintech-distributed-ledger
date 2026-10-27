-- EB.USER · operators who sign in to the console, and their sessions.
CREATE TABLE operators (
    id                   TEXT PRIMARY KEY,
    version              INTEGER NOT NULL DEFAULT 0,
    username             TEXT NOT NULL UNIQUE,
    name                 TEXT NOT NULL,
    email                TEXT NOT NULL,
    permissions          TEXT NOT NULL,
    active               INTEGER NOT NULL,
    password_hash        TEXT NOT NULL,
    must_change_password INTEGER NOT NULL,
    password_changed_at  DATETIME,
    failed_logins        INTEGER NOT NULL,
    locked_until         DATETIME,
    last_login_at        DATETIME,
    created_at           DATETIME NOT NULL
);

CREATE TABLE operator_sessions (
    id           TEXT PRIMARY KEY,
    version      INTEGER NOT NULL DEFAULT 0,
    operator_id  TEXT NOT NULL REFERENCES operators (id),
    token_hash   TEXT NOT NULL UNIQUE,
    created_at   DATETIME NOT NULL,
    last_seen_at DATETIME NOT NULL,
    expires_at   DATETIME NOT NULL,
    revoked_at   DATETIME,
    remote_addr  TEXT NOT NULL,
    user_agent   TEXT NOT NULL
);
CREATE INDEX ix_operator_sessions_operator ON operator_sessions (operator_id);
