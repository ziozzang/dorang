-- What the UPSTREAM stated a request cost. See the postgres migration of the
-- same number.
ALTER TABLE request_logs ADD COLUMN reported_cost_nano INTEGER;
