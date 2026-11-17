-- Drift checks: the no-change plans writ serve makes on a timer of each
-- OpenTofu stack@env it keeps state for, at the head of the default
-- branch of the stack's repository. A check is not a plan: it is never
-- approved or applied, and its saved plan is never kept. ref is the
-- check's run id, which its state token names while it plans. plan_id is
-- the id of the masked plan it made, '' when it made none. outside counts
-- the resources OpenTofu found changed outside OpenTofu, code the changes
-- an apply of the code would make. changes is a JSON list of the changed
-- resources, from the masked plan: address, kind, action, and the names
-- of the attributes that change, never a value. drifted_since is when the
-- first of an unbroken run of drifted checks of the stack@env ended, 0
-- for a check that found none. Each stack@env keeps its newest checks
-- only.
CREATE TABLE drift_checks (
  ref            text PRIMARY KEY,
  env            text NOT NULL,
  stack          text NOT NULL,
  status         text NOT NULL CONSTRAINT drift_checks_status CHECK (status IN ('planning', 'clean', 'drifted', 'failed')),
  started_at     bigint NOT NULL,
  made_at        bigint NOT NULL DEFAULT 0,
  plan_id        text NOT NULL DEFAULT '',
  repo           text NOT NULL DEFAULT '',
  source_commit  text NOT NULL DEFAULT '',
  outside        integer NOT NULL DEFAULT 0,
  code           integer NOT NULL DEFAULT 0,
  changes        text NOT NULL DEFAULT '[]',
  error          text NOT NULL DEFAULT '',
  drifted_since  bigint NOT NULL DEFAULT 0
);
CREATE INDEX drift_checks_stack ON drift_checks (env, stack, started_at DESC);
CREATE INDEX drift_checks_plan ON drift_checks (plan_id);
