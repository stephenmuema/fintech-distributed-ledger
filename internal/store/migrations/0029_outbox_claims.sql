-- With several instances, each outbox event is delivered by one dispatcher at a time:
-- a dispatcher claims a batch until claimed_until, and another takes back what it didn't finish.
ALTER TABLE outbox_messages ADD COLUMN claimed_by TEXT;
ALTER TABLE outbox_messages ADD COLUMN claimed_until DATETIME;
CREATE INDEX ix_outbox_claimed ON outbox_messages (claimed_by);
