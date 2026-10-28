-- Several instances.
--
-- job_leases: a job only one instance should run (scheduled COB, the operations sweeps) runs on
-- the instance holding its lease, renewed while it runs; another takes it over once it expires.
CREATE TABLE job_leases (
    name       TEXT PRIMARY KEY,
    holder     TEXT NOT NULL,
    expires_at DATETIME NOT NULL
);

-- config_versions: bumped in the commit that authorises a parameter change, so every instance
-- reloads what it keeps in memory (providers, extensions, table rates), not only the one that
-- authorised it.
CREATE TABLE config_versions (
    name    TEXT PRIMARY KEY,
    version INTEGER NOT NULL
);
