-- Utilization pricing disclosure on the ledger row (DESIGN §8.6). See the sqlite
-- migration of the same number for what each column is and why all three are nullable.
--
-- A plain ADD COLUMN on a partitioned table propagates to every partition and needs no
-- per-partition step, exactly as 0004's secret_id did.
ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS util_multiplier_ppm BIGINT;
ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS util_ppm BIGINT;
ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS util_source TEXT;
