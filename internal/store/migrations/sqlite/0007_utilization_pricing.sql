-- Utilization pricing disclosure on the ledger row (DESIGN §8.6).
--
-- A price that moves with a measurement is a price a caller cannot predict, and the only
-- thing that makes it disputable is the row carrying BOTH the factor that was applied and
-- where the reading came from. The response headers publish the same three values; these
-- columns are what an invoice is reconciled against months later, when the headers are
-- gone.
--
-- All three are nullable and every one of them is NULL on an ordinary rate card. That is
-- the honest encoding: a rule that does not price on utilization has no factor, and a zero
-- would claim one.
--
-- util_source is the reason text and not a boolean, because the fallbacks are the whole
-- point. `observed` and `no_load_header` produce the SAME charge on an idle backend and
-- are not the same fact about it — one is a measurement, the other is its absence, and
-- VLLM.md §3.1 exists because the two are indistinguishable from their numbers alone.
ALTER TABLE request_logs ADD COLUMN util_multiplier_ppm INTEGER;
ALTER TABLE request_logs ADD COLUMN util_ppm INTEGER;
ALTER TABLE request_logs ADD COLUMN util_source TEXT;
