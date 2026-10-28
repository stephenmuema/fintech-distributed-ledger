-- A wallet by its ledger accounts: a posting looks up the wallet behind each customer account it
-- writes (to refuse money for a closed account), and statements and the GL go from a
-- ledger account to its wallet. Without these, every such lookup read the whole wallets table.
CREATE INDEX ix_wallets_available ON wallets (available_account_id);
CREATE INDEX ix_wallets_held ON wallets (held_account_id);
