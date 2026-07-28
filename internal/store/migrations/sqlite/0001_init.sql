-- dorang schema, SQLite dialect (DESIGN 9.2).
--
-- Logically identical to migrations/postgres/0001_init.sql. The differences are
-- exactly three, and all three are forced by the engine:
--   * BOOLEAN     -> INTEGER 0/1. database/sql converts both directions, so Go
--                    code binds and scans a plain bool against either dialect.
--   * BYTEA       -> BLOB.
--   * PARTITION BY-> absent. SQLite has no partitions, so retention is a
--                    bounded DELETE instead of a DROP TABLE. The API says which
--                    mechanism ran rather than pretending they are the same
--                    thing (see Store.Maintain / MaintenanceReport.Mechanism).
--
-- Timestamps are BIGINT unix microseconds UTC and money is BIGINT nano-units,
-- exactly as in the PostgreSQL dialect. See that file for why.

CREATE TABLE users (
    id              TEXT    PRIMARY KEY,
    email           TEXT    NOT NULL,
    name            TEXT    NOT NULL DEFAULT '',
    role            TEXT    NOT NULL DEFAULT 'internal_user',
    max_budget_nano INTEGER,
    budget_period   TEXT,
    budget_reset_at INTEGER,
    spend_nano      INTEGER NOT NULL DEFAULT 0,
    rpm_limit       INTEGER,
    tpm_limit       INTEGER,
    models          TEXT    NOT NULL DEFAULT '[]',
    blocked         INTEGER NOT NULL DEFAULT 0,
    metadata        TEXT    NOT NULL DEFAULT '{}',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE UNIQUE INDEX users_email_key ON users (email);

CREATE TABLE teams (
    id              TEXT    PRIMARY KEY,
    name            TEXT    NOT NULL DEFAULT '',
    alias           TEXT,
    organization_id TEXT,
    max_budget_nano INTEGER,
    budget_period   TEXT,
    budget_reset_at INTEGER,
    spend_nano      INTEGER NOT NULL DEFAULT 0,
    rpm_limit       INTEGER,
    tpm_limit       INTEGER,
    max_parallel    INTEGER,
    models          TEXT    NOT NULL DEFAULT '[]',
    blocked         INTEGER NOT NULL DEFAULT 0,
    metadata        TEXT    NOT NULL DEFAULT '{}',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE TABLE team_members (
    team_id         TEXT    NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id         TEXT    NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role            TEXT    NOT NULL DEFAULT 'member',
    max_budget_nano INTEGER,
    spend_nano      INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    PRIMARY KEY (team_id, user_id)
);

CREATE TABLE api_keys (
    id                   TEXT    PRIMARY KEY,
    lookup               TEXT    NOT NULL,
    token_hash           TEXT    NOT NULL,
    hash_scheme          TEXT    NOT NULL CHECK (hash_scheme IN ('dorang_v1', 'legacy_sha256')),
    key_label            TEXT    NOT NULL,
    key_alias            TEXT,
    user_id              TEXT,
    team_id              TEXT,
    models               TEXT    NOT NULL DEFAULT '[]',
    allowed_routes       TEXT    NOT NULL DEFAULT '[]',
    object_permission_id TEXT,
    max_budget_nano      INTEGER,
    soft_budget_nano     INTEGER,
    budget_period        TEXT,
    budget_reset_at      INTEGER,
    spend_nano           INTEGER NOT NULL DEFAULT 0,
    rpm_limit            INTEGER,
    tpm_limit            INTEGER,
    max_parallel         INTEGER,
    priority_class       TEXT    NOT NULL DEFAULT 'default',
    tags                 TEXT    NOT NULL DEFAULT '[]',
    blocked              INTEGER NOT NULL DEFAULT 0,
    expires_at           INTEGER,
    source               TEXT    NOT NULL DEFAULT 'native',
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
);

CREATE UNIQUE INDEX api_keys_lookup_key ON api_keys (lookup);

CREATE TABLE providers (
    id              TEXT    PRIMARY KEY,
    name            TEXT    NOT NULL,
    kind            TEXT    NOT NULL,
    base_url        TEXT    NOT NULL DEFAULT '',
    timeout_ms      INTEGER,
    max_concurrency INTEGER,
    capacity_group  TEXT,
    params          TEXT    NOT NULL DEFAULT '{}',
    retry           TEXT    NOT NULL DEFAULT '{}',
    usage_probe     TEXT    NOT NULL DEFAULT '{}',
    enabled         INTEGER NOT NULL DEFAULT 1,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE UNIQUE INDEX providers_name_key ON providers (name);

CREATE TABLE credentials (
    id              TEXT    PRIMARY KEY,
    provider_id     TEXT    NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    name            TEXT    NOT NULL DEFAULT '',
    key_ref         TEXT    NOT NULL DEFAULT '',
    capacity_group  TEXT,
    max_concurrency INTEGER,
    priority        INTEGER NOT NULL DEFAULT 0,
    weight          INTEGER NOT NULL DEFAULT 1,
    enabled         INTEGER NOT NULL DEFAULT 1,
    metadata        TEXT    NOT NULL DEFAULT '{}',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE TABLE deployments (
    id                TEXT    PRIMARY KEY,
    model_group       TEXT    NOT NULL,
    provider_id       TEXT    NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    upstream_model    TEXT    NOT NULL,
    credential_ids    TEXT    NOT NULL DEFAULT '[]',
    weight            INTEGER NOT NULL DEFAULT 1,
    priority          INTEGER NOT NULL DEFAULT 0,
    rpm_limit         INTEGER,
    tpm_limit         INTEGER,
    max_parallel      INTEGER,
    timeout_ms        INTEGER,
    stream_timeout_ms INTEGER,
    params            TEXT    NOT NULL DEFAULT '{}',
    enabled           INTEGER NOT NULL DEFAULT 1,
    created_at        INTEGER NOT NULL,
    updated_at        INTEGER NOT NULL
);

CREATE TABLE model_aliases (
    alias       TEXT    PRIMARY KEY,
    model_group TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE model_classes (
    class       TEXT    NOT NULL,
    model_group TEXT    NOT NULL,
    ordinal     INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (class, model_group)
);

CREATE TABLE pricing_rules (
    id               TEXT    PRIMARY KEY,
    rule_class       TEXT    NOT NULL CHECK (rule_class IN ('marginal_usage', 'fixed_subscription', 'adjustment')),
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

-- Ledger. No partitions here; (ts, id) is still the primary key and still the
-- keyset pagination key, so the Go queries are byte-identical to PostgreSQL's.
CREATE TABLE request_logs (
    ts                     INTEGER NOT NULL,
    id                     TEXT    NOT NULL,
    api_key_id             TEXT,
    user_id                TEXT,
    team_id                TEXT,
    credential_id          TEXT,
    provider_id            TEXT,
    deployment_id          TEXT,
    model_group            TEXT    NOT NULL DEFAULT '',
    upstream_model         TEXT    NOT NULL DEFAULT '',
    endpoint               TEXT    NOT NULL DEFAULT '',
    status                 INTEGER NOT NULL DEFAULT 0,
    error_class            TEXT,
    prompt_tokens          INTEGER NOT NULL DEFAULT 0,
    completion_tokens      INTEGER NOT NULL DEFAULT 0,
    cached_tokens          INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens       INTEGER NOT NULL DEFAULT 0,
    total_tokens           INTEGER NOT NULL DEFAULT 0,
    cost_nano              INTEGER NOT NULL DEFAULT 0,
    marginal_cost_nano     INTEGER NOT NULL DEFAULT 0,
    subscription_cost_nano INTEGER NOT NULL DEFAULT 0,
    latency_ms             INTEGER NOT NULL DEFAULT 0,
    ttft_ms                INTEGER NOT NULL DEFAULT 0,
    queue_ms               INTEGER NOT NULL DEFAULT 0,
    capacity_wait_ms       INTEGER NOT NULL DEFAULT 0,
    upstream_ms            INTEGER NOT NULL DEFAULT 0,
    fallback_count         INTEGER NOT NULL DEFAULT 0,
    streamed               INTEGER NOT NULL DEFAULT 0,
    trace_id               TEXT,
    session_id             TEXT,
    node_id                TEXT,
    batch_id               TEXT,
    metadata               TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (ts, id)
);

CREATE INDEX request_logs_key_ts_idx   ON request_logs (api_key_id, ts DESC, id DESC);
CREATE INDEX request_logs_team_ts_idx  ON request_logs (team_id, ts DESC, id DESC);
CREATE INDEX request_logs_cred_ts_idx  ON request_logs (credential_id, ts DESC, id DESC);
CREATE INDEX request_logs_trace_ts_idx ON request_logs (trace_id, ts DESC, id DESC);
CREATE INDEX request_logs_errors_idx   ON request_logs (ts DESC, id DESC) WHERE status >= 400;

CREATE TABLE request_log_tags (
    ts         INTEGER NOT NULL,
    request_id TEXT    NOT NULL,
    tag        TEXT    NOT NULL,
    PRIMARY KEY (ts, request_id, tag)
);

CREATE INDEX request_log_tags_tag_ts_idx ON request_log_tags (tag, ts DESC, request_id DESC);

CREATE TABLE request_traces (
    ts            INTEGER NOT NULL,
    request_id    TEXT    NOT NULL,
    trace_id      TEXT,
    excerpt       TEXT    NOT NULL DEFAULT '',
    excerpt_bytes INTEGER NOT NULL DEFAULT 0,
    excerpt_mode  TEXT    NOT NULL DEFAULT 'truncated',
    spans         TEXT    NOT NULL DEFAULT '[]',
    PRIMARY KEY (ts, request_id)
);

CREATE TABLE usage_by_key_hour (
    bucket_start      INTEGER NOT NULL,
    api_key_id        TEXT    NOT NULL,
    requests          INTEGER NOT NULL DEFAULT 0,
    errors            INTEGER NOT NULL DEFAULT 0,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    cached_tokens     INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    cost_nano         INTEGER NOT NULL DEFAULT 0,
    latency_ms_sum    INTEGER NOT NULL DEFAULT 0,
    updated_at        INTEGER NOT NULL,
    PRIMARY KEY (bucket_start, api_key_id)
);

CREATE TABLE usage_by_model_hour (
    bucket_start      INTEGER NOT NULL,
    model_group       TEXT    NOT NULL,
    requests          INTEGER NOT NULL DEFAULT 0,
    errors            INTEGER NOT NULL DEFAULT 0,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    cached_tokens     INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    cost_nano         INTEGER NOT NULL DEFAULT 0,
    latency_ms_sum    INTEGER NOT NULL DEFAULT 0,
    updated_at        INTEGER NOT NULL,
    PRIMARY KEY (bucket_start, model_group)
);

CREATE TABLE usage_by_team_day (
    bucket_start      INTEGER NOT NULL,
    team_id           TEXT    NOT NULL,
    requests          INTEGER NOT NULL DEFAULT 0,
    errors            INTEGER NOT NULL DEFAULT 0,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    cached_tokens     INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    cost_nano         INTEGER NOT NULL DEFAULT 0,
    latency_ms_sum    INTEGER NOT NULL DEFAULT 0,
    updated_at        INTEGER NOT NULL,
    PRIMARY KEY (bucket_start, team_id)
);

-- "window" is quoted here purely to keep the SQL identical to PostgreSQL's,
-- where it is a reserved word.
CREATE TABLE quota_buckets (
    scope        TEXT    NOT NULL,
    scope_key    TEXT    NOT NULL,
    "window"     TEXT    NOT NULL,
    metric       TEXT    NOT NULL,
    bucket_start INTEGER NOT NULL,
    value        INTEGER NOT NULL DEFAULT 0,
    updated_at   INTEGER NOT NULL,
    PRIMARY KEY (scope, scope_key, "window", metric, bucket_start)
);

CREATE TABLE quota_leases (
    id          TEXT    PRIMARY KEY,
    node_id     TEXT    NOT NULL,
    scope       TEXT    NOT NULL,
    scope_key   TEXT    NOT NULL,
    "window"    TEXT    NOT NULL,
    metric      TEXT    NOT NULL,
    amount      INTEGER NOT NULL,
    used        INTEGER NOT NULL DEFAULT 0,
    acquired_at INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL
);

CREATE TABLE budget_state (
    subject_kind   TEXT    NOT NULL,
    subject_id     TEXT    NOT NULL,
    period         TEXT    NOT NULL,
    period_start   INTEGER NOT NULL,
    spent_nano     INTEGER NOT NULL DEFAULT 0,
    reserved_nano  INTEGER NOT NULL DEFAULT 0,
    reserved_until INTEGER,
    updated_at     INTEGER NOT NULL,
    PRIMARY KEY (subject_kind, subject_id, period, period_start)
);

CREATE TABLE credential_state (
    credential_id        TEXT    PRIMARY KEY,
    health               TEXT    NOT NULL DEFAULT 'healthy',
    unavailable_until    INTEGER,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    quota_snapshot       TEXT    NOT NULL DEFAULT '{}',
    updated_at           INTEGER NOT NULL
);

CREATE TABLE responses_store (
    response_id          TEXT    PRIMARY KEY,
    created_at           INTEGER NOT NULL,
    expires_at           INTEGER NOT NULL,
    owner_key_id         TEXT,
    previous_response_id TEXT,
    model_group          TEXT    NOT NULL DEFAULT '',
    items                TEXT    NOT NULL DEFAULT '[]',
    reasoning_blobs      BLOB
);

CREATE INDEX responses_store_expiry_idx ON responses_store (expires_at);

CREATE TABLE nodes (
    node_id        TEXT    PRIMARY KEY,
    address        TEXT    NOT NULL DEFAULT '',
    version        TEXT    NOT NULL DEFAULT '',
    started_at     INTEGER NOT NULL,
    last_heartbeat INTEGER NOT NULL,
    is_leader      INTEGER NOT NULL DEFAULT 0,
    metadata       TEXT    NOT NULL DEFAULT '{}'
);

CREATE TABLE capacity_leases (
    id          TEXT    PRIMARY KEY,
    node_id     TEXT    NOT NULL,
    axis        TEXT    NOT NULL,
    axis_key    TEXT    NOT NULL,
    amount      INTEGER NOT NULL,
    used        INTEGER NOT NULL DEFAULT 0,
    acquired_at INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL
);

CREATE TABLE files (
    id           TEXT    PRIMARY KEY,
    purpose      TEXT    NOT NULL DEFAULT '',
    filename     TEXT    NOT NULL DEFAULT '',
    bytes        INTEGER NOT NULL DEFAULT 0,
    sha256       TEXT    NOT NULL DEFAULT '',
    owner_key_id TEXT,
    storage_ref  TEXT    NOT NULL DEFAULT '',
    status       TEXT    NOT NULL DEFAULT 'uploaded',
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER
);

CREATE TABLE batches (
    id                   TEXT    PRIMARY KEY,
    owner_key_id         TEXT,
    endpoint             TEXT    NOT NULL DEFAULT '',
    input_file_id        TEXT,
    output_file_id       TEXT,
    error_file_id        TEXT,
    completion_window_ms INTEGER NOT NULL DEFAULT 0,
    status               TEXT    NOT NULL DEFAULT 'validating',
    total_count          INTEGER NOT NULL DEFAULT 0,
    completed_count      INTEGER NOT NULL DEFAULT 0,
    failed_count         INTEGER NOT NULL DEFAULT 0,
    metadata             TEXT    NOT NULL DEFAULT '{}',
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    in_progress_at       INTEGER,
    finalizing_at        INTEGER,
    completed_at         INTEGER,
    failed_at            INTEGER,
    cancelled_at         INTEGER,
    expires_at           INTEGER
);

CREATE TABLE batch_requests (
    batch_id       TEXT    NOT NULL REFERENCES batches(id) ON DELETE CASCADE,
    custom_id      TEXT    NOT NULL,
    seq            INTEGER NOT NULL,
    prefix_hash    TEXT    NOT NULL DEFAULT '',
    model_group    TEXT    NOT NULL DEFAULT '',
    status         TEXT    NOT NULL DEFAULT 'queued',
    attempts       INTEGER NOT NULL DEFAULT 0,
    request_log_id TEXT,
    error          TEXT,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    PRIMARY KEY (batch_id, custom_id)
);

CREATE TABLE audit_logs (
    id           TEXT    PRIMARY KEY,
    ts           INTEGER NOT NULL,
    actor_kind   TEXT    NOT NULL DEFAULT '',
    actor_id     TEXT    NOT NULL DEFAULT '',
    action       TEXT    NOT NULL DEFAULT '',
    object_kind  TEXT    NOT NULL DEFAULT '',
    object_id    TEXT    NOT NULL DEFAULT '',
    before_state TEXT,
    after_state  TEXT,
    ip           TEXT    NOT NULL DEFAULT '',
    user_agent   TEXT    NOT NULL DEFAULT ''
);
