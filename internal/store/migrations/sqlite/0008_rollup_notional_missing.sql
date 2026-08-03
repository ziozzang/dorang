-- The half of DESIGN §8.5 the rollups could not express.
--
-- Migration 0002 gave every usage_by_* materialization a notional_nano column,
-- and it stayed at zero on every deployment: nothing wrote it, and the reader
-- reported the notional figure as unavailable because a sum with no way to say
-- "this is complete" is not a figure anyone may print. Two of the six tiles on
-- the operator's usage screen were therefore permanently dead — an honest
-- `unavailable`, in every window, forever, which is furniture rather than an
-- answer.
--
-- A sum alone cannot carry that answer, and this is why there are two counts
-- rather than one:
--
--   notional_requests  requests in this bucket that were priced at list rate.
--   notional_missing   requests that reached PRICING and had no notional_rate
--                      rule to price with.
--
-- The figure is complete when notional_requests > 0 and notional_missing = 0.
-- Each count exists to refuse a different lie:
--
--   * Without notional_missing, a bucket where one request had no list rate
--     would print a sum that is short by an unknown amount — §8.5 rule 5's
--     flattering understatement, presented as a total.
--   * Without notional_requests, a bucket written BEFORE this column existed —
--     requests > 0, both counts defaulted to 0 — would read as "complete, and
--     the list rate came to nothing", which is the same lie about historical
--     data. It now reads as unavailable, which is the truth about it.
--
-- Neither counts a request that never reached pricing: a refusal before
-- dispatch has no usage to price at list rate, and counting it would make every
-- window containing a single 4xx report its whole notional total unavailable —
-- the tile back to being furniture, with more machinery behind it.
ALTER TABLE usage_by_key_hour   ADD COLUMN notional_requests INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_by_key_hour   ADD COLUMN notional_missing  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_by_model_hour ADD COLUMN notional_requests INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_by_model_hour ADD COLUMN notional_missing  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_by_team_day   ADD COLUMN notional_requests INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_by_team_day   ADD COLUMN notional_missing  INTEGER NOT NULL DEFAULT 0;
