-- The subscription period accumulator, made durable (DESIGN §8.1).
--
-- The SQLite file of the same version carries the full rationale. What differs
-- here is spelling only: BIGINT for the microsecond instant.

CREATE TABLE subscription_state (
    rule_id         TEXT   PRIMARY KEY,
    period_start    BIGINT NOT NULL,
    attributed_atto TEXT   NOT NULL DEFAULT '0',
    updated_at      BIGINT NOT NULL
);

ALTER TABLE usage_by_key_hour   ADD COLUMN IF NOT EXISTS marginal_nano     BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage_by_key_hour   ADD COLUMN IF NOT EXISTS subscription_nano BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage_by_model_hour ADD COLUMN IF NOT EXISTS marginal_nano     BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage_by_model_hour ADD COLUMN IF NOT EXISTS subscription_nano BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage_by_team_day   ADD COLUMN IF NOT EXISTS marginal_nano     BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage_by_team_day   ADD COLUMN IF NOT EXISTS subscription_nano BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS usage_by_key_hour_key_idx ON usage_by_key_hour (api_key_id, bucket_start);
