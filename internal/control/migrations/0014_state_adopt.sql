-- When a person last adopted the state of a stack@env: writ state adopt
-- sealed a secret set with a state version, or an S3 serial, that no
-- apply of writ serve sealed one with. 0 when no one did. A drift check
-- that started before it is out of date, so the stack@env is due again.
ALTER TABLE state_index_stacks ADD COLUMN adopted_at bigint NOT NULL DEFAULT 0;

-- One row per stack@env that an apply began or an adoption sealed a set
-- for. Both lock the row, so an adoption commits either before an apply
-- of its stack@env begins or after it saw that apply, on either backend.
CREATE TABLE stack_turns (
  env   text NOT NULL,
  stack text NOT NULL,
  PRIMARY KEY (env, stack)
);
