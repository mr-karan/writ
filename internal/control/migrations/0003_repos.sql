-- Repositories the control plane reads code from, the commits it read, and
-- the files of those commits, stored once by their Git blob id. No row holds
-- a forge credential: a repository names a forge, whose token is a file the
-- config names.

-- Each repository: a project at a forge and the path in it that holds
-- writ.estate. project_id is the forge's numeric id, which later calls use,
-- so a renamed project keeps its row. Under writ serve --dev, kind dir names
-- a local Git repository by its absolute path.
CREATE TABLE repos (
  name            text PRIMARY KEY,
  forge           text NOT NULL,          -- the forge's name in the config, or dir
  kind            text NOT NULL CHECK (kind IN ('gitlab', 'dir')),
  forge_url       text NOT NULL,          -- the forge's URL when added; '' for dir
  project         text NOT NULL,          -- the path the forge last reported, such as acme/infra
  project_id      text NOT NULL,
  path            text NOT NULL,          -- '' for the project's root
  prefix          text NOT NULL,          -- '' or a stack name prefix ending in /
  default_branch  text NOT NULL,
  web_url         text NOT NULL,
  origin          text NOT NULL CHECK (origin IN ('api', 'config')),
  added_by        text NOT NULL,          -- the person or principal; config for a pinned row
  added_at        bigint NOT NULL,
  head            text NOT NULL DEFAULT '', -- the newest commit of the default branch read
  head_read_at    bigint,
  polled_at       bigint,
  error           text NOT NULL DEFAULT '', -- why the last poll or read failed; '' after one that worked
  error_at        bigint
);
CREATE UNIQUE INDEX repos_project ON repos (kind, forge_url, project_id, path);

-- Each commit read, with its tree under the repository's path and what
-- discovery found there.
CREATE TABLE repo_commits (
  repo         text NOT NULL REFERENCES repos(name) ON DELETE CASCADE,
  sha          text NOT NULL,
  read_at      bigint NOT NULL,
  tree         text NOT NULL,             -- JSON: [{"path": "network/main.writ", "blob": "..."}], every regular file
  files        integer NOT NULL,          -- the estate's own files fetched
  bytes        bigint NOT NULL,
  stacks       text NOT NULL,             -- JSON: the stacks discovery found
  diagnostics  text NOT NULL,             -- JSON: what discovery reported
  PRIMARY KEY (repo, sha)
);

-- File contents by Git blob id, which the control plane checked against the
-- content before it stored it. Two commits that share a file share its row.
CREATE TABLE blobs (
  id         text PRIMARY KEY,
  size       bigint NOT NULL,
  content    {{blob}} NOT NULL,
  stored_at  bigint NOT NULL
);

-- What a plan was made from: a commit of a repository, or, with all three
-- empty, sources the client sent, which no team's control plane applies.
ALTER TABLE plans ADD COLUMN repo text NOT NULL DEFAULT '';
ALTER TABLE plans ADD COLUMN source_ref text NOT NULL DEFAULT '';
ALTER TABLE plans ADD COLUMN source_commit text NOT NULL DEFAULT '';
CREATE INDEX plans_commit ON plans (repo, source_commit) WHERE source_commit <> '';
