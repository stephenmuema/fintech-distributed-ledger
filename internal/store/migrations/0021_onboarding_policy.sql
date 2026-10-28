-- The onboarding policy: how customers sign up, one record amended by administrators
-- under maker-checker. Until one is authorised, KYC is required.
CREATE TABLE onboarding_policies (
    id           TEXT PRIMARY KEY,
    version      INTEGER NOT NULL DEFAULT 0,
    code         TEXT NOT NULL UNIQUE,
    kyc_required INTEGER NOT NULL,
    reason       TEXT NOT NULL,
    updated_by   TEXT NOT NULL,
    updated_at   DATETIME NOT NULL
);
