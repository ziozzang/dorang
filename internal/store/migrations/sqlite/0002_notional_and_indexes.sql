-- Three things the first migration could not carry.
--
-- The notional figure (DESIGN §8.5) records what traffic would have cost at
-- list price, so a subscription's value is visible rather than hidden behind a
-- flat bill. It was designed, priced, and surfaced in a header — but the schema
-- had nowhere to put it, and the pricing CHECK actively rejected the rule
-- class, so the feature could not be stored at all. Found by wiring the admin
-- API against it.

-- Pricing rules of class notional_rate must be storable.
-- SQLite cannot drop a CHECK constraint, so the table is rebuilt. The column
-- list is spelled out rather than copied with SELECT *, because a positional
-- copy silently mismatches the moment either side gains a column — which is
-- what happened on the first attempt at this migration.
PRAGMA foreign_keys=OFF;

CREATE TABLE pricing_rules_new (
    id               TEXT    PRIMARY KEY,
    rule_class       TEXT    NOT NULL CHECK (rule_class IN ('marginal_usage', 'fixed_subscription', 'adjustment', 'notional_rate')),
    scope_kind       TEXT    NOT NULL,
    scope_key        TEXT    NOT NULL DEFAULT '',
    provider_id      TEXT,
    credential_id    TEXT,
    deployment_id    TEXT,
    model_group      TEXT,
    model_prefix     TEXT,
    priority         INTEGER NOT NULL DEFAULT 0,
    currency         TEXT    NOT NULL DEFAULT 'USD',
    components       TEXT    NOT NULL DEFAULT '{}',
    period           TEXT,
    period_cost_nano INTEGER,
    effective_from   INTEGER,
    effective_to     INTEGER,
    enabled          INTEGER NOT NULL DEFAULT 1,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);

INSERT INTO pricing_rules_new (id, rule_class, scope_kind, scope_key, provider_id, credential_id, deployment_id, model_group, model_prefix, priority, currency, components, period, period_cost_nano, effective_from, effective_to, enabled, created_at, updated_at)
SELECT id, rule_class, scope_kind, scope_key, provider_id, credential_id, deployment_id, model_group, model_prefix, priority, currency, components, period, period_cost_nano, effective_from, effective_to, enabled, created_at, updated_at FROM pricing_rules;

DROP TABLE pricing_rules;
ALTER TABLE pricing_rules_new RENAME TO pricing_rules;

PRAGMA foreign_keys=ON;

-- The figure itself, kept in its own column everywhere cost is kept. It is
-- never folded into cost_nano: §8.5 requires that exclusion be structural, and
-- a shared column would make it a matter of remembering.
--
-- The _known flag exists because "missing" and "zero" are different answers.
-- A subscription with no notional rate would otherwise look infinitely
-- efficient, which is the most flattering possible reading and the one least
-- likely to be questioned.
ALTER TABLE request_logs        ADD COLUMN notional_nano INTEGER NOT NULL DEFAULT 0;
ALTER TABLE request_logs        ADD COLUMN notional_known INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_by_key_hour   ADD COLUMN notional_nano INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_by_model_hour ADD COLUMN notional_nano INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_by_team_day   ADD COLUMN notional_nano INTEGER NOT NULL DEFAULT 0;

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
    ts            INTEGER NOT NULL,
    credential_id TEXT NOT NULL,
    from_health   TEXT NOT NULL DEFAULT '',
    to_health     TEXT NOT NULL,
    reason        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS credential_health_history_cred_ts_idx
    ON credential_health_history (credential_id, ts DESC, id DESC);
CREATE INDEX IF NOT EXISTS credential_health_history_ts_idx
    ON credential_health_history (ts DESC, id DESC);
