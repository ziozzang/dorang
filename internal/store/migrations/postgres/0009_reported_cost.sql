-- What the UPSTREAM stated a request cost, beside dorang's own figure
-- (cost_nano). NULL is a vendor that stated nothing; 0 is a vendor that said
-- the request was free. request_logs is partitioned by day, and ADD COLUMN on
-- the parent reaches every partition.
ALTER TABLE request_logs ADD COLUMN IF NOT EXISTS reported_cost_nano BIGINT;
