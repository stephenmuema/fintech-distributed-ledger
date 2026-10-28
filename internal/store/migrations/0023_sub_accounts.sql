-- Sub-accounts: a main account's holder (say, the parent of a family visiting Kenya)
-- opens accounts in the same currency for the people travelling with them. The customer stays
-- one record — one KYC, one set of limits — and each sub-account names who uses it.
ALTER TABLE wallets ADD COLUMN parent_wallet_id TEXT;
ALTER TABLE wallets ADD COLUMN holder_name TEXT;
ALTER TABLE wallets ADD COLUMN holder_relationship TEXT;
CREATE INDEX ix_wallets_parent ON wallets (parent_wallet_id);
