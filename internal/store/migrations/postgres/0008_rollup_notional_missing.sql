-- The half of DESIGN §8.5 the rollups could not express. See the sqlite
-- migration of the same number for what each count is, and why a complete
-- notional figure needs both of them rather than a sum on its own.
--
-- The usage_by_* tables are not partitioned, so this is one ADD COLUMN each.
ALTER TABLE usage_by_key_hour   ADD COLUMN IF NOT EXISTS notional_requests BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage_by_key_hour   ADD COLUMN IF NOT EXISTS notional_missing  BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage_by_model_hour ADD COLUMN IF NOT EXISTS notional_requests BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage_by_model_hour ADD COLUMN IF NOT EXISTS notional_missing  BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage_by_team_day   ADD COLUMN IF NOT EXISTS notional_requests BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage_by_team_day   ADD COLUMN IF NOT EXISTS notional_missing  BIGINT NOT NULL DEFAULT 0;
