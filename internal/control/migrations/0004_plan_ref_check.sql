-- How a commit plan's ref was checked: on-ref when the forge showed that
-- the commit is the ref's commit or an ancestor of it, no-ref when the plan
-- names no ref, and empty for a speculative plan.
ALTER TABLE plans ADD COLUMN source_ref_check text NOT NULL DEFAULT '';
