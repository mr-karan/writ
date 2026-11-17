-- The key of each apply request, and the process that began each run.
--
-- writ apply --remote sends one key with every request of one command. A
-- key has at most one row, and the row is final: started, with the run
-- that a request with the key began, or none, which the outcome route
-- writes for a key with no row. issued_at is the time the key carries.
-- writ serve refuses a key older than its life, so it deletes a none row
-- only after that. A started row stays as long as its run.
--
-- run_id is checked at the commit, because BeginApply inserts the key's
-- row first, before the run.
CREATE TABLE apply_requests (
  request_key text PRIMARY KEY,
  outcome     text NOT NULL CONSTRAINT apply_requests_outcome CHECK (outcome IN ('started', 'none')),
  run_id      text UNIQUE REFERENCES runs(id) DEFERRABLE INITIALLY DEFERRED,
  -- The principal and the SHA-256 of the request's fields of a started
  -- row, '' for none.
  issuer      text NOT NULL DEFAULT '',
  subject     text NOT NULL DEFAULT '',
  request     text NOT NULL DEFAULT '',
  issued_at   bigint NOT NULL,
  at          bigint NOT NULL,
  CONSTRAINT apply_requests_run CHECK ((outcome = 'started') = (run_id IS NOT NULL))
);
CREATE INDEX apply_requests_none ON apply_requests (issued_at) WHERE outcome = 'none';

-- The greatest time of a key whose none row was deleted. It only grows. A
-- key at or below it starts no run, even when the clock of writ serve
-- moved back, because its none row can be gone.
CREATE TABLE apply_key_floor (
  id        integer PRIMARY KEY CONSTRAINT apply_key_floor_one CHECK (id = 1),
  issued_at bigint NOT NULL
);

-- The id of the writ serve process that began the run. A run left
-- applying by another process, or by this one with no work behind it, is
-- failed. A run from before this migration has ''.
ALTER TABLE runs ADD COLUMN owner text NOT NULL DEFAULT '';
