-- Busy shared accounts split into parts on the server engines: provider floats and
-- books, fee income and payouts in transit. A part is a full ledger account in the same category,
-- GL line and currency, pointing at the account it belongs to; the account's balance is the sum.
ALTER TABLE ledger_accounts ADD COLUMN parent_id TEXT;
CREATE INDEX ix_ledger_accounts_parent ON ledger_accounts (parent_id);
