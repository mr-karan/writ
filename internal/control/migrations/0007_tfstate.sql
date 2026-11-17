-- The http state backend that writ serve provides to OpenTofu runs.
--
-- Each run gets a token, kept only as its SHA-256. A plan's token reads the
-- state of its one stack@env; an apply's token also locks and writes it.
-- A token ends at expires_at, or when its run ends and its rows go.
CREATE TABLE tfstate_tokens (
  hash        {{blob}} PRIMARY KEY,
  run_id      text NOT NULL,
  env         text NOT NULL,
  stack       text NOT NULL,
  scope       text NOT NULL CONSTRAINT tfstate_tokens_scope CHECK (scope IN ('plan', 'apply')),
  created_at  bigint NOT NULL,
  expires_at  bigint NOT NULL
);
CREATE INDEX tfstate_tokens_run ON tfstate_tokens (run_id);
CREATE INDEX tfstate_tokens_expires ON tfstate_tokens (expires_at);

-- Each state a run wrote, sealed under the environment's key. version
-- counts from 1 for each stack@env. serial and lineage are the two keys
-- OpenTofu keeps outside its own encryption; sha256 is of the bytes
-- OpenTofu sent. Only the newest versions of each stack@env are kept.
CREATE TABLE tfstate_versions (
  env         text NOT NULL,
  stack       text NOT NULL,
  version     bigint NOT NULL,
  serial      bigint NOT NULL,
  lineage     text NOT NULL,
  sha256      text NOT NULL,
  run_id      text NOT NULL,
  created_at  bigint NOT NULL,
  sealed      {{blob}} NOT NULL,
  PRIMARY KEY (env, stack, version)
);

-- The lock of each stack@env, held by one run. info is the lock's JSON as
-- OpenTofu sent it, which a refused LOCK returns.
CREATE TABLE tfstate_locks (
  env         text NOT NULL,
  stack       text NOT NULL,
  lock_id     text NOT NULL,
  run_id      text NOT NULL,
  info        text NOT NULL,
  created_at  bigint NOT NULL,
  PRIMARY KEY (env, stack)
);
CREATE INDEX tfstate_locks_run ON tfstate_locks (run_id);
