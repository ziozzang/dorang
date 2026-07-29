-- The subscription period accumulator, made durable (DESIGN §8.1).
--
--   subscription_state    how much of an open fixed_subscription period's plan
--                         cost has already been attributed to ledger rows.
--
-- §8.1's invariant is that "the shares a period attributes sum to the plan cost,
-- and never to more". The accumulator that enforces it lived in the loaded price
-- catalog and nowhere else, which made it survive a configuration reload (the
-- catalog adopts the previous one's state) and NOT a process restart. Measured
-- on a 100.00 USD monthly plan across two process starts: **180.46 USD
-- attributed**, with the second start's whole 90.23 USD elapsed share landing on
-- the first request after the restart — which the budget gate then reserves
-- against that request's own key, so a brand-new key with a 1.00 USD ceiling was
-- refused `400 budget_exceeded` on its first ever request.
--
-- One row per rule id, not one per (rule, node): a random node id is generated
-- per process when `cluster.node_id` is unset, so a per-node row would be
-- written by a process that never reads it again — exactly the restart this
-- table exists for. Sharing the row also bounds the FLEET rather than each node
-- separately, which is the other half of the same defect (N nodes, N
-- accumulators, N plan costs).
--
-- attributed_atto is decimal DIGITS and not a number. The value is atto-scaled —
-- a 100 USD plan is 10^20 atto — which is five times larger than a 64-bit
-- integer holds, and the alternatives were a pair of limbs (exact, illegible) or
-- a float (legible, not exact). Nothing in this schema is allowed to be the
-- second kind of wrong about money.
--
-- The merge is monotone and it is done in Go, not in SQL: a period that has
-- moved on replaces the row, the same period keeps the LARGER attributed total,
-- and an older period never pulls a newer one backwards. Two nodes checkpointing
-- at once therefore converge on the high-water mark instead of overwriting each
-- other, and the loser of the race is corrected on its next tick.

CREATE TABLE subscription_state (
    rule_id         TEXT    PRIMARY KEY,
    -- period_start is unix microseconds, as every other instant in this schema.
    period_start    INTEGER NOT NULL,
    attributed_atto TEXT    NOT NULL DEFAULT '0',
    updated_at      INTEGER NOT NULL
);

-- The cost decomposition, in the rollups as well as in the ledger row.
--
--   {usage_by_key_hour, usage_by_model_hour, usage_by_team_day}
--       .marginal_nano, .subscription_nano
--
-- DESIGN §8.1: "The two are separate fields, never conflated." request_logs has
-- carried the pair since the first migration; the rollups carried one
-- `cost_nano`, and /global/spend/report answered `marginal_spend` from it — so a
-- flat plan's share was reported as marginal usage on every aggregate. Routing
-- compares the marginal figure precisely so a sunk plan cost cannot make a
-- saturated plan look cheap, and an operator comparing models by
-- `marginal_spend` was being handed the plan.
--
-- Two columns and not one. The three classes compose as `marginal +
-- subscription, then adjustments in order`, so `cost_nano - subscription_nano`
-- is the marginal figure only when no adjustment rule exists. A derived column
-- that is right until someone writes a discount is the shape this schema keeps
-- refusing.
--
-- Existing rows keep 0/0 rather than being backfilled from cost_nano. There is
-- nothing to backfill FROM — the split was never recorded — and writing
-- `marginal_nano = cost_nano` would restate history as a measurement that was
-- never taken.
ALTER TABLE usage_by_key_hour   ADD COLUMN marginal_nano     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_by_key_hour   ADD COLUMN subscription_nano INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_by_model_hour ADD COLUMN marginal_nano     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_by_model_hour ADD COLUMN subscription_nano INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_by_team_day   ADD COLUMN marginal_nano     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_by_team_day   ADD COLUMN subscription_nano INTEGER NOT NULL DEFAULT 0;

-- One key's window, as a seek rather than a scan.
--
-- usage_by_key_hour is keyed (bucket_start, api_key_id), which serves the
-- report that walks a window across every key. It does not serve the other
-- question — ONE key across a window — and that question is now asked by
-- /key/info and /key/list, whose `spend` field had no producer at all before:
-- `api_keys.spend_nano` is read there and written by nothing on the request
-- path, so every key reported `"spend": 0` while its own ledger rows, its
-- response headers and /global/spend/report all agreed on another number.
--
-- The durable budget counter was the other candidate and is the wrong one: a
-- node charges a whole lease block to `budget_state` before it spends a unit of
-- it, so that column runs up to one block ahead of reality by design. This
-- rollup is what agrees with the ledger.
CREATE INDEX usage_by_key_hour_key_idx ON usage_by_key_hour (api_key_id, bucket_start);
