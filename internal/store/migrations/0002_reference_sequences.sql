-- Per-day, per-prefix counters for T24-style transaction references (e.g. FT26301K3QZ8).
-- Random suffixes collide at volume (32^5 per prefix per day, birthday-bounded); a counter
-- scrambled by a bijection cannot.
CREATE TABLE reference_sequences (
    booking_date TEXT NOT NULL,
    prefix       TEXT NOT NULL,
    next_value   INTEGER NOT NULL,
    PRIMARY KEY (booking_date, prefix)
);
