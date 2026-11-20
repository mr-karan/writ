-- A plan whose apply would change nothing ends no-changes, a status the
-- CHECK of plans did not name. SQLite cannot alter a CHECK, so this
-- migration is a pair of files, one an engine. No row moves: a plan from
-- before keeps its status.
ALTER TABLE plans DROP CONSTRAINT plans_status;
ALTER TABLE plans ADD CONSTRAINT plans_status CHECK (status IN ('planning', 'pending', 'approved', 'applying', 'applied',
  'no-changes', 'refused', 'failed', 'expired', 'superseded'));
