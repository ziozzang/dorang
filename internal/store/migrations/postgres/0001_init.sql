-- dorang schema, PostgreSQL dialect (DESIGN 9.2).
--
-- Conventions shared with the SQLite dialect, so that one set of Go queries
-- serves both:
--   * every timestamp is BIGINT unix MICROSECONDS in UTC. SQLite has no
--     timestamp type, so one of the two dialects has to give; an integer is
--     exact, orders correctly, carries no time zone, and makes daily partition
--     bounds computable arithmetic rather than a locale question.
--   * every money amount is BIGINT nano-units of the rule's currency. Prices
--     are computed in exact decimal with 128-bit intermediates (DESIGN 8.3) and
--     rounded ONCE; what lands here is the already-rounded settled amount.
--     Writers range-check before writing so overflow is an error, never a
--     negative cost.
--   * list-valued columns are JSON text. They are never queried by content --
--     authorization runs off an in-memory snapshot (DESIGN 9.1) -- so there is
--     nothing to index and nothing to gain from a native array type that
--     SQLite cannot mirror.

CREATE TABLE users (
    id              TEXT   PRIMARY KEY,
    email           TEXT   NOT NULL,
    name            TEXT   NOT NULL DEFAULT '',
    role            TEXT   NOT NULL DEFAULT 'internal_user',
    max_budget_nano BIGINT,
    budget_period   TEXT,
    budget_reset_at BIGINT,
    spend_nano      BIGINT NOT NULL DEFAULT 0,
    rpm_limit       BIGINT,
    tpm_limit       BIGINT,
    models          TEXT   NOT NULL DEFAULT '[]',
    blocked         BOOLEAN NOT NULL DEFAULT FALSE,
    metadata        TEXT   NOT NULL DEFAULT '{}',
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL
);

-- Identity constraint, not a query optimization.
CREATE UNIQUE INDEX users_email_key ON users (email);

CREATE TABLE teams (
    id              TEXT   PRIMARY KEY,
    name            TEXT   NOT NULL DEFAULT '',
    alias           TEXT,
    organization_id TEXT,
    max_budget_nano BIGINT,
    budget_period   TEXT,
    budget_reset_at BIGINT,
    spend_nano      BIGINT NOT NULL DEFAULT 0,
    rpm_limit       BIGINT,
    tpm_limit       BIGINT,
    max_parallel    BIGINT,
    models          TEXT   NOT NULL DEFAULT '[]',
    blocked         BOOLEAN NOT NULL DEFAULT FALSE,
    metadata        TEXT   NOT NULL DEFAULT '{}',
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL
);

CREATE TABLE team_members (
    team_id         TEXT   NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id         TEXT   NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role            TEXT   NOT NULL DEFAULT 'member',
    max_budget_nano BIGINT,
    spend_nano      BIGINT NOT NULL DEFAULT 0,
    created_at      BIGINT NOT NULL,
    PRIMARY KEY (team_id, user_id)
);

-- api_keys.lookup is sha256(token)[:16] in hex: a scheme-INDEPENDENT index key
-- (DESIGN 2.4). Two hash schemes must still be one lookup, so the unique index
-- lives on lookup and never on token_hash.
--
-- There is deliberately no foreign key on user_id / team_id. Authentication
-- must not fail because of referential noise, and the importer establishes its
-- own referential rule (see ImportOptions.OnMissingTeam) which is stricter than
-- a cascade: a key whose team limits cannot be resolved is refused, not
-- orphaned, because an orphaned key fails OPEN.
CREATE TABLE api_keys (
    id                   TEXT   PRIMARY KEY,
    lookup               TEXT   NOT NULL,
    token_hash           TEXT   NOT NULL,
    hash_scheme          TEXT   NOT NULL CHECK (hash_scheme IN ('dorang_v1', 'legacy_sha256')),
    key_label            TEXT   NOT NULL,
    key_alias            TEXT,
    user_id              TEXT,
    team_id              TEXT,
    models               TEXT   NOT NULL DEFAULT '[]',
    allowed_routes       TEXT   NOT NULL DEFAULT '[]',
    object_permission_id TEXT,
    max_budget_nano      BIGINT,
    soft_budget_nano     BIGINT,
    budget_period        TEXT,
    budget_reset_at      BIGINT,
    spend_nano           BIGINT NOT NULL DEFAULT 0,
    rpm_limit            BIGINT,
    tpm_limit            BIGINT,
    max_parallel         BIGINT,
    priority_class       TEXT   NOT NULL DEFAULT 'default',
    tags                 TEXT   NOT NULL DEFAULT '[]',
    blocked              BOOLEAN NOT NULL DEFAULT FALSE,
    expires_at           BIGINT,
    source               TEXT   NOT NULL DEFAULT 'native',
    created_at           BIGINT NOT NULL,
    updated_at           BIGINT NOT NULL
);

CREATE UNIQUE INDEX api_keys_lookup_key ON api_keys (lookup);

CREATE TABLE providers (
    id              TEXT   PRIMARY KEY,
    name            TEXT   NOT NULL,
    kind            TEXT   NOT NULL,
    base_url        TEXT   NOT NULL DEFAULT '',
    timeout_ms      BIGINT,
    max_concurrency BIGINT,
    capacity_group  TEXT,
    params          TEXT   NOT NULL DEFAULT '{}',
    retry           TEXT   NOT NULL DEFAULT '{}',
    usage_probe     TEXT   NOT NULL DEFAULT '{}',
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL
);

CREATE UNIQUE INDEX providers_name_key ON providers (name);

-- key_ref is an indirection (env / file / external ref), never a secret.
-- DESIGN 4.1: secrets never appear in configuration, and by extension never in
-- the configuration overlay that this table is.
CREATE TABLE credentials (
    id              TEXT   PRIMARY KEY,
    provider_id     TEXT   NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    name            TEXT   NOT NULL DEFAULT '',
    key_ref         TEXT   NOT NULL DEFAULT '',
    capacity_group  TEXT,
    max_concurrency BIGINT,
    priority        BIGINT NOT NULL DEFAULT 0,
    weight          BIGINT NOT NULL DEFAULT 1,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    metadata        TEXT   NOT NULL DEFAULT '{}',
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL
);

CREATE TABLE deployments (
    id                TEXT   PRIMARY KEY,
    model_group       TEXT   NOT NULL,
    provider_id       TEXT   NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
    upstream_model    TEXT   NOT NULL,
    credential_ids    TEXT   NOT NULL DEFAULT '[]',
    weight            BIGINT NOT NULL DEFAULT 1,
    priority          BIGINT NOT NULL DEFAULT 0,
    rpm_limit         BIGINT,
    tpm_limit         BIGINT,
    max_parallel      BIGINT,
    timeout_ms        BIGINT,
    stream_timeout_ms BIGINT,
    params            TEXT   NOT NULL DEFAULT '{}',
    enabled           BOOLEAN NOT NULL DEFAULT TRUE,
    created_at        BIGINT NOT NULL,
    updated_at        BIGINT NOT NULL
);

CREATE TABLE model_aliases (
    alias       TEXT   PRIMARY KEY,
    model_group TEXT   NOT NULL,
    created_at  BIGINT NOT NULL,
    updated_at  BIGINT NOT NULL
);

CREATE TABLE model_classes (
    class       TEXT   NOT NULL,
    model_group TEXT   NOT NULL,
    ordinal     BIGINT NOT NULL DEFAULT 0,
    created_at  BIGINT NOT NULL,
    PRIMARY KEY (class, model_group)
);

-- components holds exact decimal STRINGS (DESIGN 8.3). Storing a rate as a
-- float would reintroduce exactly the binary-rounding error the pricing section
-- exists to avoid.
CREATE TABLE pricing_rules (
    id               TEXT   PRIMARY KEY,
    rule_class       TEXT   NOT NULL CHECK (rule_class IN ('marginal_usage', 'fixed_subscription', 'adjustment')),
    scope_kind       TEXT   NOT NULL,
    scope_key        TEXT   NOT NULL DEFAULT '',
    provider_id      TEXT,
    credential_id    TEXT,
    deployment_id    TEXT,
    model_group      TEXT,
    model_prefix     TEXT,
    priority         BIGINT NOT NULL DEFAULT 0,
    currency         TEXT   NOT NULL DEFAULT 'USD',
    components       TEXT   NOT NULL DEFAULT '{}',
    period           TEXT,
    period_cost_nano BIGINT,
    effective_from   BIGINT,
    effective_to     BIGINT,
    enabled          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at       BIGINT NOT NULL,
    updated_at       BIGINT NOT NULL
);

-- ---------------------------------------------------------------------------
-- Ledger. Range-partitioned by day on ts (DESIGN 9.5).
--
-- There is deliberately NO default partition. A default partition converts the
-- "no partition for this row" failure from loud to silent, and then makes
-- attaching the real partition require a full scan of the default. The writer
-- creates partitions ahead of time and additionally recovers from a missing
-- one in-line, which is the behaviour R1-15 asks for.
--
-- The primary key must contain the partition key, so it is (ts, id) rather than
-- (id). That is also the pagination key: every ledger listing is a keyset scan
-- descending on (ts, id).
-- ---------------------------------------------------------------------------
CREATE TABLE request_logs (
    ts                     BIGINT  NOT NULL,
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
    prompt_tokens          BIGINT  NOT NULL DEFAULT 0,
    completion_tokens      BIGINT  NOT NULL DEFAULT 0,
    cached_tokens          BIGINT  NOT NULL DEFAULT 0,
    reasoning_tokens       BIGINT  NOT NULL DEFAULT 0,
    total_tokens           BIGINT  NOT NULL DEFAULT 0,
    cost_nano              BIGINT  NOT NULL DEFAULT 0,
    marginal_cost_nano     BIGINT  NOT NULL DEFAULT 0,
    subscription_cost_nano BIGINT  NOT NULL DEFAULT 0,
    latency_ms             BIGINT  NOT NULL DEFAULT 0,
    ttft_ms                BIGINT  NOT NULL DEFAULT 0,
    queue_ms               BIGINT  NOT NULL DEFAULT 0,
    capacity_wait_ms       BIGINT  NOT NULL DEFAULT 0,
    upstream_ms            BIGINT  NOT NULL DEFAULT 0,
    fallback_count         INTEGER NOT NULL DEFAULT 0,
    streamed               BOOLEAN NOT NULL DEFAULT FALSE,
    trace_id               TEXT,
    session_id             TEXT,
    node_id                TEXT,
    batch_id               TEXT,
    metadata               TEXT    NOT NULL DEFAULT '{}',
    PRIMARY KEY (ts, id)
) PARTITION BY RANGE (ts);

-- DESIGN 9.3, one index per supported query and nothing else. The trailing id
-- is not decoration: without it the keyset predicate (ts, id) < (?, ?) cannot
-- be satisfied from the index and every page costs a sort.
CREATE INDEX request_logs_key_ts_idx   ON request_logs (api_key_id, ts DESC, id DESC);
CREATE INDEX request_logs_team_ts_idx  ON request_logs (team_id, ts DESC, id DESC);
CREATE INDEX request_logs_cred_ts_idx  ON request_logs (credential_id, ts DESC, id DESC);
CREATE INDEX request_logs_trace_ts_idx ON request_logs (trace_id, ts DESC, id DESC);
CREATE INDEX request_logs_errors_idx   ON request_logs (ts DESC, id DESC) WHERE status >= 400;

-- Tags are a normalized child table, never an array column scanned per row
-- (DESIGN 9.3). Partitioned on the same key as the parent so retention drops
-- rather than deletes, and so the join prunes on both sides.
CREATE TABLE request_log_tags (
    ts         BIGINT NOT NULL,
    request_id TEXT   NOT NULL,
    tag        TEXT   NOT NULL,
    PRIMARY KEY (ts, request_id, tag)
) PARTITION BY RANGE (ts);

CREATE INDEX request_log_tags_tag_ts_idx ON request_log_tags (tag, ts DESC, request_id DESC);

-- Excerpts live here, not inline in the ledger (DESIGN 9.2, R1-3). Sampled and
-- byte-budgeted; fetched by the (ts, request_id) of a ledger row already in
-- hand, so no additional index is warranted.
CREATE TABLE request_traces (
    ts            BIGINT NOT NULL,
    request_id    TEXT   NOT NULL,
    trace_id      TEXT,
    excerpt       TEXT   NOT NULL DEFAULT '',
    excerpt_bytes BIGINT NOT NULL DEFAULT 0,
    excerpt_mode  TEXT   NOT NULL DEFAULT 'truncated',
    spans         TEXT   NOT NULL DEFAULT '[]',
    PRIMARY KEY (ts, request_id)
) PARTITION BY RANGE (ts);

-- ---------------------------------------------------------------------------
-- Rollups (DESIGN 9.4). Purpose-built, not a full cube. Each node
-- pre-aggregates in memory and merges once per flush, so writes to a given row
-- are bounded by node count, not request count.
-- ---------------------------------------------------------------------------
CREATE TABLE usage_by_key_hour (
    bucket_start      BIGINT NOT NULL,
    api_key_id        TEXT   NOT NULL,
    requests          BIGINT NOT NULL DEFAULT 0,
    errors            BIGINT NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    cached_tokens     BIGINT NOT NULL DEFAULT 0,
    reasoning_tokens  BIGINT NOT NULL DEFAULT 0,
    total_tokens      BIGINT NOT NULL DEFAULT 0,
    cost_nano         BIGINT NOT NULL DEFAULT 0,
    latency_ms_sum    BIGINT NOT NULL DEFAULT 0,
    updated_at        BIGINT NOT NULL,
    PRIMARY KEY (bucket_start, api_key_id)
);

CREATE TABLE usage_by_model_hour (
    bucket_start      BIGINT NOT NULL,
    model_group       TEXT   NOT NULL,
    requests          BIGINT NOT NULL DEFAULT 0,
    errors            BIGINT NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    cached_tokens     BIGINT NOT NULL DEFAULT 0,
    reasoning_tokens  BIGINT NOT NULL DEFAULT 0,
    total_tokens      BIGINT NOT NULL DEFAULT 0,
    cost_nano         BIGINT NOT NULL DEFAULT 0,
    latency_ms_sum    BIGINT NOT NULL DEFAULT 0,
    updated_at        BIGINT NOT NULL,
    PRIMARY KEY (bucket_start, model_group)
);

CREATE TABLE usage_by_team_day (
    bucket_start      BIGINT NOT NULL,
    team_id           TEXT   NOT NULL,
    requests          BIGINT NOT NULL DEFAULT 0,
    errors            BIGINT NOT NULL DEFAULT 0,
    prompt_tokens     BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    cached_tokens     BIGINT NOT NULL DEFAULT 0,
    reasoning_tokens  BIGINT NOT NULL DEFAULT 0,
    total_tokens      BIGINT NOT NULL DEFAULT 0,
    cost_nano         BIGINT NOT NULL DEFAULT 0,
    latency_ms_sum    BIGINT NOT NULL DEFAULT 0,
    updated_at        BIGINT NOT NULL,
    PRIMARY KEY (bucket_start, team_id)
);

-- "window" is a reserved word in PostgreSQL and is quoted everywhere it is
-- used. The design names the column window; renaming it silently would be a
-- worse trade than quoting it in the four statements that touch it.
CREATE TABLE quota_buckets (
    scope        TEXT   NOT NULL,
    scope_key    TEXT   NOT NULL,
    "window"     TEXT   NOT NULL,
    metric       TEXT   NOT NULL,
    bucket_start BIGINT NOT NULL,
    value        BIGINT NOT NULL DEFAULT 0,
    updated_at   BIGINT NOT NULL,
    PRIMARY KEY (scope, scope_key, "window", metric, bucket_start)
);

CREATE TABLE quota_leases (
    id          TEXT   PRIMARY KEY,
    node_id     TEXT   NOT NULL,
    scope       TEXT   NOT NULL,
    scope_key   TEXT   NOT NULL,
    "window"    TEXT   NOT NULL,
    metric      TEXT   NOT NULL,
    amount      BIGINT NOT NULL,
    used        BIGINT NOT NULL DEFAULT 0,
    acquired_at BIGINT NOT NULL,
    expires_at  BIGINT NOT NULL
);

-- reserved_until is the whole point of this table (DESIGN 6.4, R1-5): a process
-- killed between reserve and settle must not lock that amount forever.
CREATE TABLE budget_state (
    subject_kind   TEXT   NOT NULL,
    subject_id     TEXT   NOT NULL,
    period         TEXT   NOT NULL,
    period_start   BIGINT NOT NULL,
    spent_nano     BIGINT NOT NULL DEFAULT 0,
    reserved_nano  BIGINT NOT NULL DEFAULT 0,
    reserved_until BIGINT,
    updated_at     BIGINT NOT NULL,
    PRIMARY KEY (subject_kind, subject_id, period, period_start)
);

CREATE TABLE credential_state (
    credential_id        TEXT   PRIMARY KEY,
    health               TEXT   NOT NULL DEFAULT 'healthy',
    unavailable_until    BIGINT,
    consecutive_failures BIGINT NOT NULL DEFAULT 0,
    quota_snapshot       TEXT   NOT NULL DEFAULT '{}',
    updated_at           BIGINT NOT NULL
);

-- DESIGN 9.2 / R1-C7. reasoning_blobs holds opaque integrity-bearing reasoning
-- handles as raw bytes so they can be replayed byte-identically.
CREATE TABLE responses_store (
    response_id          TEXT   PRIMARY KEY,
    created_at           BIGINT NOT NULL,
    expires_at           BIGINT NOT NULL,
    owner_key_id         TEXT,
    previous_response_id TEXT,
    model_group          TEXT   NOT NULL DEFAULT '',
    items                TEXT   NOT NULL DEFAULT '[]',
    reasoning_blobs      BYTEA
);

-- Serves the expiry sweep, which is an implemented query.
CREATE INDEX responses_store_expiry_idx ON responses_store (expires_at);

CREATE TABLE nodes (
    node_id        TEXT   PRIMARY KEY,
    address        TEXT   NOT NULL DEFAULT '',
    version        TEXT   NOT NULL DEFAULT '',
    started_at     BIGINT NOT NULL,
    last_heartbeat BIGINT NOT NULL,
    is_leader      BOOLEAN NOT NULL DEFAULT FALSE,
    metadata       TEXT   NOT NULL DEFAULT '{}'
);

CREATE TABLE capacity_leases (
    id          TEXT   PRIMARY KEY,
    node_id     TEXT   NOT NULL,
    axis        TEXT   NOT NULL,
    axis_key    TEXT   NOT NULL,
    amount      BIGINT NOT NULL,
    used        BIGINT NOT NULL DEFAULT 0,
    acquired_at BIGINT NOT NULL,
    expires_at  BIGINT NOT NULL
);

CREATE TABLE files (
    id           TEXT   PRIMARY KEY,
    purpose      TEXT   NOT NULL DEFAULT '',
    filename     TEXT   NOT NULL DEFAULT '',
    bytes        BIGINT NOT NULL DEFAULT 0,
    sha256       TEXT   NOT NULL DEFAULT '',
    owner_key_id TEXT,
    storage_ref  TEXT   NOT NULL DEFAULT '',
    status       TEXT   NOT NULL DEFAULT 'uploaded',
    created_at   BIGINT NOT NULL,
    expires_at   BIGINT
);

CREATE TABLE batches (
    id                   TEXT   PRIMARY KEY,
    owner_key_id         TEXT,
    endpoint             TEXT   NOT NULL DEFAULT '',
    input_file_id        TEXT,
    output_file_id       TEXT,
    error_file_id        TEXT,
    completion_window_ms BIGINT NOT NULL DEFAULT 0,
    status               TEXT   NOT NULL DEFAULT 'validating',
    total_count          BIGINT NOT NULL DEFAULT 0,
    completed_count      BIGINT NOT NULL DEFAULT 0,
    failed_count         BIGINT NOT NULL DEFAULT 0,
    metadata             TEXT   NOT NULL DEFAULT '{}',
    created_at           BIGINT NOT NULL,
    updated_at           BIGINT NOT NULL,
    in_progress_at       BIGINT,
    finalizing_at        BIGINT,
    completed_at         BIGINT,
    failed_at            BIGINT,
    cancelled_at         BIGINT,
    expires_at           BIGINT
);

CREATE TABLE batch_requests (
    batch_id       TEXT   NOT NULL REFERENCES batches(id) ON DELETE CASCADE,
    custom_id      TEXT   NOT NULL,
    seq            BIGINT NOT NULL,
    prefix_hash    TEXT   NOT NULL DEFAULT '',
    model_group    TEXT   NOT NULL DEFAULT '',
    status         TEXT   NOT NULL DEFAULT 'queued',
    attempts       BIGINT NOT NULL DEFAULT 0,
    request_log_id TEXT,
    error          TEXT,
    created_at     BIGINT NOT NULL,
    updated_at     BIGINT NOT NULL,
    PRIMARY KEY (batch_id, custom_id)
);

CREATE TABLE audit_logs (
    id           TEXT   PRIMARY KEY,
    ts           BIGINT NOT NULL,
    actor_kind   TEXT   NOT NULL DEFAULT '',
    actor_id     TEXT   NOT NULL DEFAULT '',
    action       TEXT   NOT NULL DEFAULT '',
    object_kind  TEXT   NOT NULL DEFAULT '',
    object_id    TEXT   NOT NULL DEFAULT '',
    before_state TEXT,
    after_state  TEXT,
    ip           TEXT   NOT NULL DEFAULT '',
    user_agent   TEXT   NOT NULL DEFAULT ''
);
