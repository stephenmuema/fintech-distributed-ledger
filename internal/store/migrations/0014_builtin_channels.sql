-- Built-in providers are channels: provider names the compiled-in connector of a channel.
ALTER TABLE provider_channels ADD COLUMN provider TEXT NOT NULL DEFAULT '';
ALTER TABLE provider_channels ADD COLUMN auto_clear_at INTEGER NOT NULL DEFAULT 0;
