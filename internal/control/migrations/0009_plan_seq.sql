-- seq orders the plans of one stack@env as they were requested, with no
-- ties: two plans requested in the same millisecond still have an order,
-- so only the newer one applies. A plan's insert takes the next number
-- of its stack@env from plan_seqs in the same transaction; the upsert
-- locks the row, so two inserts never take the same number. Plans made
-- before this migration are numbered by requested_at, then ref.
CREATE TABLE plan_seqs (
  env   text NOT NULL,
  stack text NOT NULL,
  seq   bigint NOT NULL,
  PRIMARY KEY (env, stack)
);
ALTER TABLE plans ADD COLUMN seq bigint;
UPDATE plans SET seq = (SELECT count(*) FROM plans o WHERE o.env = plans.env AND o.stack = plans.stack
  AND (o.requested_at < plans.requested_at OR o.requested_at = plans.requested_at AND o.ref <= plans.ref));
INSERT INTO plan_seqs (env, stack, seq) SELECT env, stack, max(seq) FROM plans GROUP BY env, stack;
CREATE UNIQUE INDEX plans_seq ON plans (env, stack, seq);
