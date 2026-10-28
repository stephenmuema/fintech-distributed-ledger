-- Journal entries sealed without their lines' running balances. With parallel writers
-- a busy system account's running balance is only known at commit, after the entry is written;
-- version 2 seals everything else — the reference, dates, and every line's account, direction and
-- amount. Version 1 (every entry before this) also covered the running balances.
ALTER TABLE journal_entries ADD COLUMN hash_version INTEGER NOT NULL DEFAULT 1;
