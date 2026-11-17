-- The policy decision of an OpenTofu plan at the plan point, and of its
-- run at the apply point, as writ.policy-decision/v1 with its messages
-- masked. NULL when writ serve judged no policy: none was configured, the
-- plan is of Writ's own engine, or OpenTofu refused it.
ALTER TABLE plans ADD COLUMN policy text;
ALTER TABLE runs ADD COLUMN policy text;
