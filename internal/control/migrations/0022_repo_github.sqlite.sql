-- A repository may be at a GitHub forge, kind github, which the CHECK of
-- repos did not name. No row moves.
--
-- SQLite cannot alter a CHECK, and remaking repos would break the foreign
-- keys of repo_commits inside the migrations' transaction. A CHECK that
-- takes one more value leaves every row valid, so the CHECK's text is
-- edited in place, as migration 0019 edits the CHECK of plans.
PRAGMA writable_schema = ON;
UPDATE sqlite_schema SET sql = replace(sql, 'CHECK (kind IN (''gitlab'', ''dir''))', 'CHECK (kind IN (''gitlab'', ''github'', ''dir''))')
  WHERE type = 'table' AND name = 'repos';
PRAGMA writable_schema = RESET;

-- The table refuses the migration unless the edit took, once. As DDL it
-- also changes the schema cookie, so every other connection reads the new
-- CHECK.
CREATE TABLE migration_0022 (ok integer NOT NULL CHECK (ok = 1));
INSERT INTO migration_0022 SELECT count(*) FROM sqlite_schema
  WHERE type = 'table' AND name = 'repos' AND sql LIKE '%CHECK (kind IN (''gitlab'', ''github'', ''dir''))%';
DROP TABLE migration_0022;
