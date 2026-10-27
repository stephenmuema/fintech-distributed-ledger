-- Hosted card checkout (Jenga Payment Gateway): where the payer's browser returns afterwards.
ALTER TABLE payments ADD COLUMN return_url TEXT;
