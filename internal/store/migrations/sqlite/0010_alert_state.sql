-- See the postgres migration of the same number.
CREATE TABLE IF NOT EXISTS alert_state (
    alert_key  TEXT    PRIMARY KEY,
    event      TEXT    NOT NULL,
    low        INTEGER NOT NULL DEFAULT 0,
    since      INTEGER NOT NULL,
    last_sent  INTEGER,
    seq        INTEGER NOT NULL DEFAULT 0,
    pending    TEXT    NOT NULL DEFAULT '',
    payload    TEXT    NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS alert_state_low_idx ON alert_state (last_sent) WHERE low;
