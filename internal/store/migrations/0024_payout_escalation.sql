-- Payouts nobody answers (ADR 0003): a payout whose provider never confirms it — the status
-- queries go unanswered, or the provider can't be asked — is escalated to operations rather than
-- left Processing forever. Its funds stay reserved: it may have been paid.
ALTER TABLE payouts ADD COLUMN escalated_at DATETIME;
CREATE INDEX ix_payouts_escalated ON payouts (escalated_at);
