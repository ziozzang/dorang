-- One row per alert condition (internal/alerts): whether it is low, when it
-- was last announced, and which transports have delivered that announcement.
-- The row is the claim: a node announces only by winning a conditional
-- UPDATE on it, so a restart or a second node never announces one crossing
-- twice, and a transport that failed is retried from here.
CREATE TABLE IF NOT EXISTS alert_state (
    alert_key  TEXT    PRIMARY KEY,
    event      TEXT    NOT NULL,
    low        BOOLEAN NOT NULL DEFAULT FALSE,
    since      BIGINT  NOT NULL,
    last_sent  BIGINT,
    seq        BIGINT  NOT NULL DEFAULT 0,
    pending    TEXT    NOT NULL DEFAULT '',
    payload    TEXT    NOT NULL DEFAULT '',
    updated_at BIGINT  NOT NULL
);
CREATE INDEX IF NOT EXISTS alert_state_low_idx ON alert_state (last_sent) WHERE low;
