-- Drop the tables that only the Writ language and its change watcher
-- used, in the same portable subset as 0001. stacks recorded where the
-- Writ state of each stack@env lived; an OpenTofu stack keeps its state
-- where its backend says, and writ serve's http backend keeps it in
-- tfstate_versions. The change tables held what the CloudTrail watcher
-- learned, and nothing writes or reads them since it was removed.
-- The indexes of the state index by id, arn, and identifier served only
-- the watcher's lookups.
-- The Writ state that a stacks row named, a schema of this database on
-- Postgres or a file under state/ beside control.db on SQLite, stays.
DROP TABLE change_targets;
DROP TABLE changes;
DROP TABLE change_groups;
DROP TABLE stacks;
DROP INDEX state_index_id;
DROP INDEX state_index_arn;
DROP INDEX state_index_identifier;
