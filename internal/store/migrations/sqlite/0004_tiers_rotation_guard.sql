-- Tiers (DESIGN §11.6), key rotation (§11.2c) and the invalidation path that
-- makes a revocation take effect (§11.2c, risk W11).
--
--   api_keys.tier           the tier is a property of the KEY, assigned by an
--                           operator. §10.5's rule is that an operator can
--                           grant urgency and a caller cannot claim it, so
--                           there is exactly one place a tier can enter from
--                           and this column is it. There is no request field
--                           that writes here.
--
--   api_keys.pended_at      the token guard's action (§11.6). It is separate
--                           from `blocked` on purpose: blocked is a decision an
--                           operator made, pended is a statistical judgement
--                           that might be wrong and that an operator releases
--                           in ONE action. Folding the guard's action into
--                           `blocked` would make the reversible thing
--                           indistinguishable from the deliberate one, in the
--                           ledger and in the support ticket.
--   api_keys.pend_reason    which condition tripped, in words, so the operator
--                           reading the row does not have to correlate it with
--                           an alert.
--
--   api_key_secrets         a key has an id and one or more SECRETS. This is
--                           the whole of §11.2c: everything else about the key
--                           -- tier, budget, spend, allow-list, rate limits,
--                           team, ledger history -- hangs off api_keys.id and
--                           is untouched by a rotation. A rotation that also
--                           reset the limits would be a re-provisioning, and an
--                           operator facing that puts it off, which is how a
--                           five-year-old secret happens.
--
--                           api_keys.lookup/token_hash/hash_scheme stay as the
--                           CURRENT secret's denormalized copy so that every
--                           existing query keeps working; the row here is the
--                           authority and the two are written in one
--                           transaction.
--
--   key_invalidations       the durable invalidation bus. A revocation, a pend
--                           and an early grace cut publish a row here; every
--                           node polls it and drops the key from its auth
--                           snapshot on receipt. The snapshot TTL becomes the
--                           FALLBACK for a node that missed the message rather
--                           than the mechanism, which is what closes W11: until
--                           this existed the guarantee was "eventually", and
--                           for a compromised key that is not a guarantee.
--
--   request_logs.secret_id  which secret authenticated the request. Both
--                           secrets work during a grace period, and an operator
--                           needs to see whether the client actually rolled
--                           BEFORE the window closes rather than finding out
--                           when it shuts. Without this column the ledger can
--                           say a key was used and not which credential the
--                           caller is still holding.

ALTER TABLE api_keys ADD COLUMN tier        TEXT NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN pended_at   INTEGER;
ALTER TABLE api_keys ADD COLUMN pend_reason TEXT NOT NULL DEFAULT '';

CREATE TABLE api_key_secrets (
    id          TEXT    PRIMARY KEY,
    key_id      TEXT    NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    -- generation counts rotations: 1 is the secret the key was issued with.
    generation  INTEGER NOT NULL DEFAULT 1,
    lookup      TEXT    NOT NULL,
    token_hash  TEXT    NOT NULL,
    hash_scheme TEXT    NOT NULL CHECK (hash_scheme IN ('dorang_v1', 'legacy_sha256')),
    key_label   TEXT    NOT NULL DEFAULT '',
    -- current: exactly one secret per key is the one a rotation would replace.
    -- It is an integer rather than a computed maximum so that the uniqueness of
    -- "one current secret per key" is an index the database enforces.
    is_current  INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,
    -- expires_at is the END OF THE GRACE PERIOD for a superseded secret. NULL
    -- means it does not expire on its own -- the current secret expires with
    -- the key and not before it.
    expires_at  INTEGER,
    -- revoked_at is an EARLY CUT, which is what a suspected compromise needs:
    -- rotate now, cut the old secret immediately, keep everything else. It is
    -- separate from expires_at so that "the grace ran out" and "an operator
    -- ended it" are distinguishable a month later.
    revoked_at  INTEGER
);

CREATE UNIQUE INDEX api_key_secrets_lookup_key ON api_key_secrets (lookup);
CREATE INDEX api_key_secrets_key_idx ON api_key_secrets (key_id, generation DESC);
-- One current secret per key. A partial unique index is the only form that says
-- this without also forbidding several retired secrets.
CREATE UNIQUE INDEX api_key_secrets_current_key ON api_key_secrets (key_id) WHERE is_current = 1;

-- Every key that already exists gets its existing secret as generation 1, so
-- the secrets table is authoritative from the moment it appears rather than
-- from the first rotation after it.
INSERT INTO api_key_secrets
    (id, key_id, generation, lookup, token_hash, hash_scheme, key_label, is_current, created_at)
SELECT id || '.1', id, 1, lookup, token_hash, hash_scheme, key_label, 1, created_at
  FROM api_keys;

CREATE TABLE key_invalidations (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    key_id     TEXT    NOT NULL,
    -- lookups is a JSON array of the index keys to drop. It is an optimization:
    -- the key_id is authoritative and always applied, because a node that
    -- learned a secret this publisher never saw must still drop it.
    lookups    TEXT    NOT NULL DEFAULT '[]',
    cause      TEXT    NOT NULL DEFAULT 'unspecified',
    created_at INTEGER NOT NULL
);

-- The subscriber's only query: everything after a watermark, in order.
CREATE INDEX key_invalidations_seq_idx ON key_invalidations (seq);
-- The pruner's only query.
CREATE INDEX key_invalidations_created_idx ON key_invalidations (created_at);

ALTER TABLE request_logs ADD COLUMN secret_id TEXT;
