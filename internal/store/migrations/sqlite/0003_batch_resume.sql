-- The columns a batch needs in order to survive a restart.
--
-- DESIGN §9.2 lists them beside the tables they belong to, and states why each
-- one exists, but the initial schema shipped `batches` and `batch_requests`
-- without them. The consequence was not a degraded batch surface -- it was no
-- persistent batch surface at all: internal/batch's Store could not be
-- implemented against these tables, so the gateway ran on the in-memory store
-- and Recover was wired to something that forgets everything at exit.
--
--   batches.errors          why a failed batch failed, which is otherwise
--                           unavailable after a restart.
--   batch_requests.response_body
--                           required for resume: the ledger stores no bodies,
--                           so without it a resumed batch has nothing to
--                           rebuild its output file from and EVERY
--                           already-finished row would be paid for a second
--                           time.
--   input_offset/input_length
--                           let the scheduler seek into the input file instead
--                           of holding a 200 MiB upload in memory for the life
--                           of the batch.
--
-- The rest are the fields the record already carries and the table had nowhere
-- to put: the capacity principal (distinct from the owning key, so the axis can
-- be charged to a user or a team), the two timestamps the wire object has and
-- the table did not, the row's own url and status code, and the error code that
-- is separate from the error message.

ALTER TABLE batches ADD COLUMN principal_id  TEXT;
ALTER TABLE batches ADD COLUMN errors        TEXT NOT NULL DEFAULT '[]';
ALTER TABLE batches ADD COLUMN cancelling_at INTEGER;
ALTER TABLE batches ADD COLUMN expired_at    INTEGER;

ALTER TABLE batch_requests ADD COLUMN url           TEXT    NOT NULL DEFAULT '';
ALTER TABLE batch_requests ADD COLUMN input_offset  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE batch_requests ADD COLUMN input_length  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE batch_requests ADD COLUMN status_code   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE batch_requests ADD COLUMN response_body BLOB;
ALTER TABLE batch_requests ADD COLUMN error_code    TEXT    NOT NULL DEFAULT '';

-- The files table renders an OpenAI file object, which carries status_details.
ALTER TABLE files ADD COLUMN status_details TEXT NOT NULL DEFAULT '';

-- §9.3: indexes come from the query set, and the batch store has exactly three
-- queries. Rows are always read in seq order for one batch; recovery reads every
-- non-terminal batch; listing pages by owner, newest first.
CREATE INDEX IF NOT EXISTS batch_requests_seq_idx ON batch_requests (batch_id, seq);
CREATE INDEX IF NOT EXISTS batches_status_idx     ON batches (status, created_at);
CREATE INDEX IF NOT EXISTS batches_owner_idx      ON batches (owner_key_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS files_owner_idx        ON files (owner_key_id, created_at DESC, id DESC);
