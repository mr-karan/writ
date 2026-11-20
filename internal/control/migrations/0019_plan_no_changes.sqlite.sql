-- A plan whose apply would change nothing ends no-changes, a status the
-- CHECK of plans did not name. No row moves: a plan from before keeps its
-- status.
--
-- SQLite cannot alter a CHECK, and remaking plans would break the foreign
-- keys of approvals, runs, and plan_events inside the migrations'
-- transaction. A CHECK that takes one more value leaves every row valid,
-- so the CHECK's text is edited in place, the way SQLite's documentation
-- gives for a change that leaves the file's content as it is.
PRAGMA writable_schema = ON;
UPDATE sqlite_schema SET sql = replace(sql, '''expired'', ''superseded''))', '''expired'', ''superseded'', ''no-changes''))')
  WHERE type = 'table' AND name = 'plans';
PRAGMA writable_schema = RESET;

-- The table refuses the migration unless the edit took, once. As DDL it
-- also changes the schema cookie, so every other connection reads the new
-- CHECK.
CREATE TABLE migration_0018 (ok integer NOT NULL CHECK (ok = 1));
INSERT INTO migration_0018 SELECT count(*) FROM sqlite_schema
  WHERE type = 'table' AND name = 'plans' AND sql LIKE '%''expired'', ''superseded'', ''no-changes''))%';
DROP TABLE migration_0018;
