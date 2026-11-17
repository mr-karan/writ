-- Stacks whose state is in their own S3 bucket, where writ serve does not
-- see a write. state_serials holds each serial of such a state that a run
-- or a refresh of writ serve read: the serials its applies wrote, and
-- those no apply of writ serve wrote, which are drift. The first serial
-- writ serve reads of a stack@env is its baseline, and no drift. Each
-- stack@env keeps its newest rows only.
CREATE TABLE state_serials (
  env           text NOT NULL,
  stack         text NOT NULL,
  lineage       text NOT NULL,
  serial        bigint NOT NULL,
  -- The apply that wrote the serial, '' when none did.
  run_id        text NOT NULL,
  -- For an apply, the serial its plan read: the apply wrote every serial
  -- of the lineage above it up to serial, as OpenTofu saves state during
  -- a long apply. For any other row, serial.
  from_serial   bigint NOT NULL,
  seen_at       bigint NOT NULL,
  -- The place of the newest read of the serial among the reads of its
  -- stack@env, from state_s3_stacks.read_seq. An index read from the
  -- serial takes it, and is stored only while it is the stack@env's
  -- newest read, so a slow refresh never replaces an index of a newer
  -- read, of whatever lineage.
  read_seq      bigint NOT NULL,
  -- The secrets the mask of the apply that wrote the serial knew, sealed
  -- under the environment's key, which a refresh of the index from this
  -- serial needs. NULL for a serial no apply wrote: the index never reads
  -- it.
  index_secrets {{blob}},
  PRIMARY KEY (env, stack, lineage, serial)
);
CREATE INDEX state_serials_seen ON state_serials (env, stack, seen_at DESC);

-- Each stack@env on S3 whose state writ serve read. read_seq is the
-- number the newest read took; each read takes the next in the same
-- transaction, and the upsert locks the row, so two reads never take the
-- same number. checked_at is when the timer last read the state, 0 when
-- it never did: each look takes those it checked longest ago first, so
-- every stack gets a turn.
CREATE TABLE state_s3_stacks (
  env        text NOT NULL,
  stack      text NOT NULL,
  read_seq   bigint NOT NULL,
  checked_at bigint NOT NULL DEFAULT 0,
  PRIMARY KEY (env, stack)
);

-- The lineage of the state the index was read from, '' for the http
-- backend, whose versions are numbered by writ serve. An S3 state's
-- serial counts within its lineage.
ALTER TABLE state_index_stacks ADD COLUMN lineage text NOT NULL DEFAULT '';
-- For a stack on S3, the read_seq of the serial the index was read from,
-- the newest read of the stack@env when it was stored; 0 for the http
-- backend.
ALTER TABLE state_index_stacks ADD COLUMN read_seq bigint NOT NULL DEFAULT 0;

-- The drift marker of a stack on S3: the newest serial of its state that
-- writ serve read and no apply of writ serve wrote, and when writ serve
-- read it. 0 when it has read none. A stack whose state was never indexed
-- gets a row of version 0 to hold it.
ALTER TABLE state_index_stacks ADD COLUMN outside_serial bigint NOT NULL DEFAULT 0;
ALTER TABLE state_index_stacks ADD COLUMN outside_at bigint NOT NULL DEFAULT 0;
