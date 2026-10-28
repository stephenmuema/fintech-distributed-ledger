-- Providers are modules: a provider channel names the module that serves it, its
-- routing priority and the capabilities a checker authorised. Channels opened for compiled-in
-- connectors keep their books; they route nothing until an operator names their module.
ALTER TABLE provider_channels ADD COLUMN module TEXT NOT NULL DEFAULT '';
ALTER TABLE provider_channels ADD COLUMN priority INTEGER NOT NULL DEFAULT 100;
-- Nullable without a default, so it is a large column on every engine (snapshots exceed 255).
ALTER TABLE provider_channels ADD COLUMN capabilities TEXT;
