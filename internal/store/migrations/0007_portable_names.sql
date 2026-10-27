-- Portable column names: KEY is reserved in MySQL and SQL Server, LINES in MySQL, DATE in Oracle. Renaming them
-- lets every query run unquoted on SQLite, PostgreSQL, MySQL, SQL Server and Oracle alike.
ALTER TABLE idempotency_records RENAME COLUMN key TO idempotency_key;
ALTER TABLE parameter_change_requests RENAME COLUMN key TO param_key;
ALTER TABLE manual_adjustments RENAME COLUMN lines TO entry_lines;
-- DATE is reserved in Oracle.
ALTER TABLE holidays RENAME COLUMN date TO holiday_date;
