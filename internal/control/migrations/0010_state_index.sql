-- The state index: each resource an OpenTofu stack's state manages, with
-- the attributes the change watchers match on and no others. Its columns
-- are the allow-list: a column for each attribute awswatch matches on, and
-- for a nomad_job's namespace and modify index. '' is an attribute the
-- state does not hold, holds as no string, or marks sensitive. The rows of
-- a stack@env come only from the mask, and are replaced together.
CREATE TABLE state_index (
  env           text NOT NULL,
  stack         text NOT NULL,
  address       text NOT NULL,
  type          text NOT NULL,
  provider      text NOT NULL,
  id            text NOT NULL,
  arn           text NOT NULL,
  identifier    text NOT NULL,
  region        text NOT NULL,
  namespace     text NOT NULL,
  modify_index  text NOT NULL,
  PRIMARY KEY (env, stack, address)
);
CREATE INDEX state_index_id ON state_index (env, id);
CREATE INDEX state_index_arn ON state_index (env, arn);
CREATE INDEX state_index_identifier ON state_index (env, identifier);

-- The state each stack@env's rows came from. backend is the kind of state
-- backend: http, the backend writ serve provides, whose version is the
-- tfstate_versions row the rows were read from.
CREATE TABLE state_index_stacks (
  env         text NOT NULL,
  stack       text NOT NULL,
  backend     text NOT NULL,
  version     bigint NOT NULL,
  indexed_at  bigint NOT NULL,
  PRIMARY KEY (env, stack)
);

-- A change that names a resource of the state index has the outcome
-- indexed: nothing is marked, since every OpenTofu plan reads every
-- resource. Both engines change a CHECK by building the table again.
CREATE TABLE change_targets_next (
  env        text NOT NULL,
  event_id   text NOT NULL,
  stack      text NOT NULL,
  addr       text NOT NULL,
  type       text NOT NULL,
  wire_type  text NOT NULL,
  object     text NOT NULL,
  outcome    text NOT NULL CHECK (outcome IN ('marked', 'already-stale', 'read-since', 'indexed')),
  PRIMARY KEY (env, event_id, stack, addr),
  FOREIGN KEY (env, event_id) REFERENCES changes(env, event_id) ON DELETE CASCADE
);
INSERT INTO change_targets_next (env, event_id, stack, addr, type, wire_type, object, outcome)
  SELECT env, event_id, stack, addr, type, wire_type, object, outcome FROM change_targets;
DROP TABLE change_targets;
ALTER TABLE change_targets_next RENAME TO change_targets;
CREATE INDEX change_targets_stack ON change_targets (stack, env, event_id);
