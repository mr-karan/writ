-- The secrets that the mask of the apply which wrote a state version
-- knew, from the run's environment and from the apply's plan, sealed
-- under the environment's key for that version. A refresh of the state
-- index seeds its mask with them, since a state can hold a value that
-- only the plan marks, as an address keyed by nonsensitive(var.NAME)
-- does. NULL when no apply's mask read a plan for the version, as for one
-- written before writ serve kept them: a refresh does not index it. They
-- go with their version.
ALTER TABLE tfstate_versions ADD COLUMN index_secrets {{blob}};
