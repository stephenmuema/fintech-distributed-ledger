-- Local tables (T24: LOCAL.TABLE / LOCAL.REF / local applications). The definitions live here;
-- the tables themselves (lt_*, pd_*, la_*) are created at runtime when a definition is
-- authorised, from DDL recorded in schema_changes.
CREATE TABLE local_tables (
    id          TEXT PRIMARY KEY,
    version     INTEGER NOT NULL DEFAULT 0,
    name        TEXT NOT NULL UNIQUE,
    kind        TEXT NOT NULL,
    table_name  TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL,
    key_label   TEXT NOT NULL,
    key_pattern TEXT NOT NULL,
    authorisers INTEGER NOT NULL,
    fields      TEXT NOT NULL,
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL
);

CREATE TABLE local_columns (
    id          TEXT PRIMARY KEY,
    version     INTEGER NOT NULL DEFAULT 0,
    table_name  TEXT NOT NULL,
    name        TEXT NOT NULL,
    column_name TEXT NOT NULL,
    status      TEXT NOT NULL,
    error       TEXT,
    created_at  DATETIME NOT NULL,
    applied_at  DATETIME,
    UNIQUE (table_name, name)
);

CREATE TABLE schema_changes (
    id         TEXT PRIMARY KEY,
    version    INTEGER NOT NULL DEFAULT 0,
    table_name TEXT NOT NULL,
    target     TEXT NOT NULL,
    kind       TEXT NOT NULL,
    statement  TEXT NOT NULL,
    status     TEXT NOT NULL,
    error      TEXT,
    created_at DATETIME NOT NULL,
    applied_at DATETIME
);
CREATE INDEX ix_schema_changes_status ON schema_changes (status, created_at);
CREATE INDEX ix_schema_changes_table ON schema_changes (table_name);
