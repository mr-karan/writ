-- Everything writ serve keeps apart from stack state, in the subset of SQL
-- that SQLite and Postgres both run. The runner in internal/sqldb writes
-- {{id}} and {{blob}} for each engine. On Postgres the tables live in the
-- schema writ_control; on SQLite in control.db.
-- Its versions count in schema_migrations, apart from the schema version of
-- Writ's state, which each stack's own state keeps in its meta table.
-- Times are Unix milliseconds that writ sets from its own clock. Lists and
-- documents are JSON in text columns. A list that a query filters on has a
-- table of its own: grant_teams and audit_stacks.
-- Every table that records a person's action keeps who, when, and how they
-- proved it, so change history and audit are SQL queries.

-- Each config writ serve has started with. The config holds no secret: keys,
-- tokens, and passwords are files it names.
CREATE TABLE configs (
  id             {{id}},
  digest         text NOT NULL UNIQUE,  -- hex SHA-256 of text
  url            text NOT NULL,
  text           text NOT NULL,
  first_loaded   bigint NOT NULL,
  last_loaded    bigint NOT NULL,
  loaded_by      text NOT NULL          -- host and process id of the last start
);

-- The teams and grants of each config, as they were.
CREATE TABLE teams (
  config_id  bigint NOT NULL REFERENCES configs(id),
  name       text NOT NULL,
  issuer     text NOT NULL,
  match      text NOT NULL,             -- [{"claim": "project_id", "values": ["4242"]}]
  PRIMARY KEY (config_id, name)
);

CREATE TABLE grants (
  config_id  bigint NOT NULL REFERENCES configs(id),
  ordinal    integer NOT NULL,          -- position in the config's grants list
  stacks     text NOT NULL,             -- JSON lists
  envs       text NOT NULL,
  can        text NOT NULL,
  risks      text NOT NULL,
  PRIMARY KEY (config_id, ordinal)
);

-- The teams each grant names.
CREATE TABLE grant_teams (
  config_id  bigint NOT NULL,
  ordinal    integer NOT NULL,
  team       text NOT NULL,
  PRIMARY KEY (config_id, ordinal, team),
  FOREIGN KEY (config_id, ordinal) REFERENCES grants (config_id, ordinal)
);
CREATE INDEX grant_teams_team ON grant_teams (team);

-- Each stack@env whose state this control plane keeps, and where: a schema
-- of this database on Postgres, a file under the data directory's state/
-- on SQLite.
CREATE TABLE stacks (
  env          text NOT NULL,
  stack        text NOT NULL,
  location     text NOT NULL UNIQUE,
  created_at   bigint NOT NULL,
  PRIMARY KEY (env, stack)
);

-- Each remote plan. ref names a plan while it runs; id is the saved plan's
-- signature, set when the plan ends.
CREATE TABLE plans (
  ref             text PRIMARY KEY,
  id              text UNIQUE,
  status          text NOT NULL CHECK (status IN ('planning', 'pending', 'refused', 'failed', 'expired', 'applying', 'applied', 'void')),
  env             text NOT NULL,
  stack           text NOT NULL,
  author_issuer   text NOT NULL,
  author_subject  text NOT NULL,
  author_person   text NOT NULL,        -- gitlab.com/user/1001; '' when the token names no person
  author_display  text NOT NULL,
  author          text NOT NULL,        -- JSON: the principal and the claims its teams matched on
  teams           text NOT NULL,        -- JSON list
  origin          text NOT NULL,        -- JSON: project, pipeline, commit, merge request
  -- The parts of origin that queries filter on.
  origin_project_id    text NOT NULL DEFAULT '',
  origin_pipeline      text NOT NULL DEFAULT '',
  origin_merge_request bigint,          -- NULL when the plan is not of a merge request
  requested_at    bigint NOT NULL,
  made_at         bigint,
  expires_at      bigint,
  risks           text NOT NULL DEFAULT '[]',
  changes         integer NOT NULL DEFAULT 0,
  source_digest   text NOT NULL,
  config_id       bigint NOT NULL REFERENCES configs(id),
  document        text,                 -- writ.plan.json/v2, redacted as writ plan --json prints it
  text            text,                 -- the plan as writ plan prints it
  saved           {{blob}},             -- the saved plan; secret params sealed; never served
  -- The id of a newer pending plan of the same stack@env from the same
  -- merge request, so an old plan's page points to it.
  superseded_by   text REFERENCES plans(id)
);
CREATE INDEX plans_env_stack ON plans (env, stack, requested_at DESC, ref);
CREATE INDEX plans_requested ON plans (requested_at DESC, ref);
CREATE INDEX plans_pending ON plans (expires_at) WHERE status = 'pending';
CREATE INDEX plans_person ON plans (author_person, requested_at DESC);
CREATE INDEX plans_pipeline ON plans (origin_project_id, origin_pipeline);
CREATE INDEX plans_merge_request ON plans (origin_project_id, origin_merge_request, env, stack)
  WHERE origin_merge_request IS NOT NULL;

-- Approvals of plans, and their withdrawal. Stage 3 of the design writes
-- them; the table is here so the schema covers what the control plane keeps.
CREATE TABLE approvals (
  id            {{id}},
  plan_id       text NOT NULL REFERENCES plans(id),
  person        text NOT NULL,
  display       text NOT NULL,
  issuer        text NOT NULL,
  subject       text NOT NULL,
  via           text NOT NULL,          -- "gitlab job 9922140", "session"
  approved_at   bigint NOT NULL,
  withdrawn_at  bigint
);
CREATE UNIQUE INDEX approvals_live ON approvals (plan_id, person) WHERE withdrawn_at IS NULL;
CREATE INDEX approvals_person ON approvals (person, approved_at DESC);

-- Applies the control plane runs. Stage 3 writes them.
CREATE TABLE runs (
  id            text PRIMARY KEY,
  plan_id       text NOT NULL REFERENCES plans(id),
  env           text NOT NULL,
  stack         text NOT NULL,
  status        text NOT NULL CHECK (status IN ('applying', 'applied', 'failed', 'cancelled')),
  actor_person  text NOT NULL,
  actor_display text NOT NULL,
  actor         text NOT NULL,          -- JSON
  role_session  text NOT NULL,
  started_at    bigint NOT NULL,
  ended_at      bigint,
  document      text                    -- writ.apply/v0 when the run ends
);
CREATE INDEX runs_env_stack ON runs (env, stack, started_at DESC);
CREATE INDEX runs_plan ON runs (plan_id);

-- Applies that skipped the approval rules, with the reason. Stage 3 writes
-- them.
CREATE TABLE overrides (
  run_id      text PRIMARY KEY REFERENCES runs(id),
  plan_id     text NOT NULL REFERENCES plans(id),
  person      text NOT NULL,
  reason      text NOT NULL CHECK (length(reason) >= 10),
  at          bigint NOT NULL
);
CREATE INDEX overrides_at ON overrides (at DESC);

-- Sessions of people, from writ login or from the browser sign-in of the
-- control UI: one table, two carriers. Only the SHA-256 of a session token
-- is kept; id names a session in GET and DELETE /v1/sessions/{id}.
CREATE TABLE sessions (
  id            text PRIMARY KEY,
  hash          {{blob}} NOT NULL UNIQUE,
  carrier       text NOT NULL CHECK (carrier IN ('cli', 'browser')),
  person        text NOT NULL,
  display       text NOT NULL,
  issuer        text NOT NULL,
  subject       text NOT NULL,
  teams         text NOT NULL,          -- JSON list
  created_at    bigint NOT NULL,
  ends_at       bigint NOT NULL,
  last_seen_at  bigint,
  ended_at      bigint,
  ended_by      text
);
CREATE INDEX sessions_person ON sessions (person, created_at DESC);
CREATE INDEX sessions_live ON sessions (ends_at) WHERE ended_at IS NULL;

-- Token ids that made a write, until the token expires: a CI token creates
-- at most one plan, one approval, and one run.
CREATE TABLE token_uses (
  issuer   text NOT NULL,
  jti      text NOT NULL,
  op       text NOT NULL,
  exp      bigint NOT NULL,
  used_at  bigint NOT NULL,
  PRIMARY KEY (issuer, jti, op)
);
CREATE INDEX token_uses_exp ON token_uses (exp);

-- One row for every API call, and for every decision the control plane
-- makes on its own, such as a plan that finished. Rows are only added,
-- except by the retention setting.
CREATE TABLE audit (
  id          {{id}},
  at          bigint NOT NULL,
  request     text NOT NULL,            -- request id, also in the X-Request-Id header and the log
  kind        text NOT NULL,            -- request, plan-finished, config-loaded, started
  method      text NOT NULL DEFAULT '',
  route       text NOT NULL DEFAULT '',
  status      integer NOT NULL DEFAULT 0,
  remote      text NOT NULL DEFAULT '',
  issuer      text NOT NULL DEFAULT '',
  subject     text NOT NULL DEFAULT '',
  person      text NOT NULL DEFAULT '',
  principal   text NOT NULL DEFAULT '',  -- the display name
  principal_kind text NOT NULL DEFAULT '', -- ci-job, person for a session, or dev
  via         text NOT NULL DEFAULT '',  -- how it proved itself: "gitlab job 9922101", "session"
  teams       text NOT NULL DEFAULT '[]', -- JSON lists, as are stacks, addrs, risks, and grants
  op          text NOT NULL DEFAULT '',
  env         text NOT NULL DEFAULT '',
  stacks      text NOT NULL DEFAULT '[]',
  plan        text NOT NULL DEFAULT '',
  run         text NOT NULL DEFAULT '',
  addrs       text NOT NULL DEFAULT '[]',
  risks       text NOT NULL DEFAULT '[]',
  decision    text NOT NULL CHECK (decision IN ('allowed', 'denied', 'auth-failed', 'refused', 'error', 'none')),
  check_failed text NOT NULL DEFAULT '', -- the token check that failed, for auth-failed
  reason      text NOT NULL DEFAULT '',
  grants      text NOT NULL DEFAULT '[]',
  config_id   bigint REFERENCES configs(id),
  elapsed_ms  integer NOT NULL DEFAULT 0
);
CREATE INDEX audit_at ON audit (at DESC);
CREATE INDEX audit_person ON audit (person, at DESC) WHERE person <> '';
CREATE INDEX audit_env ON audit (env, at DESC) WHERE env <> '';
CREATE INDEX audit_plan ON audit (plan) WHERE plan <> '';
CREATE INDEX audit_run ON audit (run) WHERE run <> '';
CREATE INDEX audit_refusals ON audit (at DESC) WHERE decision NOT IN ('allowed', 'none');

-- The stacks each audit row names, for the rows of one stack.
CREATE TABLE audit_stacks (
  audit_id  bigint NOT NULL REFERENCES audit(id) ON DELETE CASCADE,
  stack     text NOT NULL,
  PRIMARY KEY (audit_id, stack)
);
CREATE INDEX audit_stacks_stack ON audit_stacks (stack, audit_id DESC);
