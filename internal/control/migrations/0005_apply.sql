-- Applies and approvals of kept plans: the plan's lifecycle with its
-- approved and superseded states, the approvals, the runs the control
-- plane applies, and each change of a plan's status.
--
-- A table's CHECK cannot change on SQLite, so plans is made again under
-- the same name. approvals, runs, and overrides held no row before this
-- migration, because no build wrote them, so they are dropped first, which
-- leaves plans referenced only by itself, and made again in their new
-- shape. overrides goes: a run keeps its override's reason.
DROP TABLE overrides;
DROP TABLE runs;
DROP TABLE approvals;

CREATE TABLE plans_new (
  ref             text PRIMARY KEY,
  id              text UNIQUE,
  -- The lifecycle. Its legal moves are the table in internal/control/lifecycle.go.
  status          text NOT NULL CONSTRAINT plans_status CHECK (status IN ('planning', 'pending', 'approved', 'applying', 'applied',
                    'refused', 'failed', 'expired', 'superseded')),
  env             text NOT NULL,
  stack           text NOT NULL,
  author_issuer   text NOT NULL,
  author_subject  text NOT NULL,
  author_person   text NOT NULL,
  author_display  text NOT NULL,
  author          text NOT NULL,
  teams           text NOT NULL,
  origin          text NOT NULL,
  -- The URL of the CI token's issuer, one GitLab instance or one GitHub.
  -- A project, pipeline, or merge request id is unique only within it, so
  -- every query by them names it too. '' for a plan of a person or of a
  -- build before this migration, which no CI job owns.
  origin_issuer        text NOT NULL DEFAULT '',
  origin_project_id    text NOT NULL DEFAULT '',
  origin_pipeline      text NOT NULL DEFAULT '',
  origin_merge_request bigint,
  requested_at    bigint NOT NULL,
  made_at         bigint,
  expires_at      bigint,
  risks           text NOT NULL DEFAULT '[]',
  changes         integer NOT NULL DEFAULT 0,
  source_digest   text NOT NULL,
  config_id       bigint NOT NULL REFERENCES configs(id),
  document        text,
  text            text,
  saved           {{blob}},
  superseded_by   text REFERENCES plans_new(id),
  repo            text NOT NULL DEFAULT '',
  source_ref      text NOT NULL DEFAULT '',
  source_commit   text NOT NULL DEFAULT '',
  source_ref_check text NOT NULL DEFAULT '',
  -- The approvals the rules asked for when the plan was made, and the
  -- 1-based positions of those rules in the config's approvals.
  need            integer NOT NULL DEFAULT 0,
  need_rules      text NOT NULL DEFAULT '[]',
  -- The writ.lock the plan was made with, which its apply runs with.
  lock            text NOT NULL DEFAULT '',
  -- Why the plan was superseded, '' otherwise.
  superseded_why  text NOT NULL DEFAULT ''
);

INSERT INTO plans_new (ref, id, status, env, stack, author_issuer, author_subject, author_person, author_display, author, teams, origin,
    origin_project_id, origin_pipeline, origin_merge_request, requested_at, made_at, expires_at, risks, changes, source_digest, config_id,
    document, text, saved, superseded_by, repo, source_ref, source_commit, source_ref_check)
  SELECT ref, id, CASE WHEN status = 'void' THEN 'superseded' ELSE status END, env, stack, author_issuer, author_subject, author_person,
    author_display, author, teams, origin, origin_project_id, origin_pipeline, origin_merge_request, requested_at, made_at, expires_at,
    risks, changes, source_digest, config_id, document, text, saved, superseded_by, repo, source_ref, source_commit, source_ref_check
  FROM plans;

DROP TABLE plans;
ALTER TABLE plans_new RENAME TO plans;

CREATE INDEX plans_env_stack ON plans (env, stack, requested_at DESC, ref);
CREATE INDEX plans_requested ON plans (requested_at DESC, ref);
CREATE INDEX plans_live ON plans (env, stack, expires_at) WHERE status IN ('pending', 'approved');
CREATE INDEX plans_person ON plans (author_person, requested_at DESC);
CREATE INDEX plans_pipeline ON plans (origin_issuer, origin_project_id, origin_pipeline);
CREATE INDEX plans_merge_request ON plans (origin_issuer, origin_project_id, origin_merge_request, env, stack)
  WHERE origin_merge_request IS NOT NULL;
CREATE INDEX plans_commit ON plans (repo, source_commit) WHERE source_commit <> '';
-- An apply after a merge looks up the live plans of a stack@env by their
-- sources.
CREATE INDEX plans_sources ON plans (repo, env, stack, source_digest) WHERE status IN ('pending', 'approved');

-- Approvals of plans, and their withdrawal. A person counts once for a
-- plan, by the stable id of person.
CREATE TABLE approvals (
  id            {{id}},
  plan_id       text NOT NULL REFERENCES plans(id),
  person        text NOT NULL,
  display       text NOT NULL,
  issuer        text NOT NULL,
  subject       text NOT NULL,
  via           text NOT NULL,          -- "gitlab job 9922140", "browser session", "dev token"
  approved_at   bigint NOT NULL,
  withdrawn_at  bigint
);
CREATE UNIQUE INDEX approvals_live ON approvals (plan_id, person) WHERE withdrawn_at IS NULL;
CREATE INDEX approvals_person ON approvals (person, approved_at DESC);

-- Applies the control plane runs. id is also the run's id in the stack's
-- state. A plan applies at most once, and one stack@env applies one plan
-- at a time: the two unique indexes hold that against every race.
CREATE TABLE runs (
  id            text PRIMARY KEY,
  plan_id       text NOT NULL REFERENCES plans(id),
  env           text NOT NULL,
  stack         text NOT NULL,
  status        text NOT NULL CONSTRAINT runs_status CHECK (status IN ('applying', 'applied', 'failed')),
  actor_issuer  text NOT NULL,
  actor_subject text NOT NULL,
  actor_person  text NOT NULL,
  actor_display text NOT NULL,
  actor         text NOT NULL,          -- JSON: the principal and the claims its teams matched on
  via           text NOT NULL,
  approvals     text NOT NULL,          -- JSON: the live approvals when the run started
  override      text NOT NULL DEFAULT '' CONSTRAINT runs_override CHECK (override = '' OR length(override) >= 10),
  -- How the request named the plan: by its id, or by a merged commit
  -- whose sources equal the plan's.
  named_by      text NOT NULL CONSTRAINT runs_named_by CHECK (named_by IN ('plan', 'merged')),
  merged_commit text NOT NULL DEFAULT '',
  role_session  text NOT NULL DEFAULT '',
  started_at    bigint NOT NULL,
  ended_at      bigint,
  document      text,                   -- writ.apply/v0 when the run ends
  text          text                    -- the apply as writ apply prints it
);
CREATE UNIQUE INDEX runs_plan ON runs (plan_id);
CREATE UNIQUE INDEX runs_applying ON runs (env, stack) WHERE status = 'applying';
CREATE INDEX runs_env_stack ON runs (env, stack, started_at DESC);

-- Each change of a plan's status, with who or what made it and why: the
-- plan's own audit trail beside the audit rows of the requests.
CREATE TABLE plan_events (
  id           {{id}},
  plan_ref     text NOT NULL REFERENCES plans(ref),
  at           bigint NOT NULL,
  from_status  text NOT NULL,
  to_status    text NOT NULL,
  person       text NOT NULL DEFAULT '',  -- '' for a change the control plane made on its own
  display      text NOT NULL DEFAULT '',
  run          text NOT NULL DEFAULT '',
  reason       text NOT NULL DEFAULT ''
);
CREATE INDEX plan_events_plan ON plan_events (plan_ref, id);

-- No build before this migration had approvals, so no plan it made can
-- have been approved, and its author's person id came from the issuer's
-- host, where another spelling of the same host named the same person
-- another way. Every live plan it made expires, with the reason in its
-- events: plan again. Its author fields stay as they were.
INSERT INTO plan_events (plan_ref, at, from_status, to_status, reason)
  SELECT ref, {{now}}, status, 'expired', 'made before approvals' FROM plans WHERE status IN ('planning', 'pending', 'approved');
UPDATE plans SET status = 'expired' WHERE status IN ('planning', 'pending', 'approved');
