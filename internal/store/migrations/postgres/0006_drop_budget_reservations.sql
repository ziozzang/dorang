-- The budget reservation columns, removed (DESIGN §6.4, §9.6).
--
-- The SQLite file of the same version carries the full rationale. What differs
-- here is spelling only: IF EXISTS, so a re-run is a no-op.
ALTER TABLE budget_state DROP COLUMN IF EXISTS reserved_nano;
ALTER TABLE budget_state DROP COLUMN IF EXISTS reserved_until;
