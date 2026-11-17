-- A plan of an OpenTofu stack keeps the version of its stack's state on
-- the http backend that the plan read, 0 when the stack had none. NULL
-- marks a plan of Writ's own engine. An apply is refused when the newest
-- state version is no longer this one. The saved plan of an OpenTofu
-- plan is sealed under the environment's key and is deleted when the
-- plan stops being live: applied, failed, superseded, or expired.
ALTER TABLE plans ADD COLUMN tofu_state bigint;
