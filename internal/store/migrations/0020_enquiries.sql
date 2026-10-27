-- Enquiries (T24: ENQUIRY): bank-defined, saved queries over transactions, customers, accounts
-- and local applications — selection, columns and sort held as data, run by the core's own
-- query builder (never raw SQL).
CREATE TABLE enquiries (
    id          TEXT PRIMARY KEY,
    version     INTEGER NOT NULL DEFAULT 0,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL,
    application TEXT NOT NULL,
    criteria    TEXT NOT NULL,
    output      TEXT NOT NULL,
    max_rows    INTEGER NOT NULL,
    created_at  DATETIME NOT NULL,
    updated_at  DATETIME NOT NULL
);
