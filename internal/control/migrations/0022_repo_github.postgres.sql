-- A repository may be at a GitHub forge, kind github, which the CHECK of
-- repos did not name. SQLite cannot alter a CHECK, so this migration is a
-- pair of files, one an engine. No row moves. The CHECK was written
-- inline on the column, so Postgres named it repos_kind_check.
ALTER TABLE repos DROP CONSTRAINT repos_kind_check;
ALTER TABLE repos ADD CONSTRAINT repos_kind_check CHECK (kind IN ('gitlab', 'github', 'dir'));
