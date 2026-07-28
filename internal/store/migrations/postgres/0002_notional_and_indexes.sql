-- Three things the first migration could not carry.
--
-- The notional figure (DESIGN §8.5) records what traffic would have cost at
-- list price, so a subscription's value is visible rather than hidden behind a
-- flat bill. It was designed, priced, and surfaced in a header — but the schema
-- had nowhere to put it, and the pricing CHECK actively rejected the rule
-- class, so the feature could not be stored at all. Found by wiring the admin
-- API against it.

-- Pricing rules of class notional_rate must be storable.
ALTER TABLE pricing_rules DROP CONSTRAINT IF EXISTS pricing_rules_rule_class_check;
ALTER TABLE pricing_rules ADD CONSTRAINT pricing_rules_rule_class_check
    CHECK (rule_class IN ('marginal_usage','fixed_subscription','adjustment','notional_rate'));

-- The figure itself, kept in its own column everywhere cost is kept. It is
-- never folded into cost_nano: §8.5 requires that exclusion be structural, and
-- a shared column would make it a matter of remembering.
--
-- The _known flag exists because "missing" and "zero" are different answers.
-- A subscription with no notional rate would otherwise look infinitely
-- efficient, which is the most flattering possible reading and the one least
-- likely to be questioned.
ALTER TABLE request_logs        ADD COLUMN notional_nano BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_logs        ADD COLUMN notional_known BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE usage_by_key_hour   ADD COLUMN notional_nano BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage_by_model_hour ADD COLUMN notional_nano BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage_by_team_day   ADD COLUMN notional_nano BIGINT NOT NULL DEFAULT 0;

-- §9.3 derives indexes from the query set, and two queries the admin API is
-- required to serve had none. Both would have degraded to the partition scan
-- that section exists to prevent.
CREATE INDEX IF NOT EXISTS request_logs_user_ts_idx ON request_logs (user_id, ts DESC, id DESC);

-- Reading the audit trail back was unindexed, so the endpoint would have been
-- shipped slow rather than shipped correct.
CREATE INDEX IF NOT EXISTS audit_logs_ts_idx    ON audit_logs (ts DESC, id DESC);
CREATE INDEX IF NOT EXISTS audit_logs_actor_idx ON audit_logs (actor_id, ts DESC, id DESC);

-- A current-state row cannot answer "what happened". credential_state holds the
-- present health; this holds the transitions, so /health/history returns a
-- record rather than an empty list that reads as "nothing ever failed".
CREATE TABLE IF NOT EXISTS credential_health_history (
    id            TEXT PRIMARY KEY,
    ts            BIGINT NOT NULL,
    credential_id TEXT NOT NULL,
    from_health   TEXT NOT NULL DEFAULT '',
    to_health     TEXT NOT NULL,
    reason        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS credential_health_history_cred_ts_idx
    ON credential_health_history (credential_id, ts DESC, id DESC);
CREATE INDEX IF NOT EXISTS credential_health_history_ts_idx
    ON credential_health_history (ts DESC, id DESC);
