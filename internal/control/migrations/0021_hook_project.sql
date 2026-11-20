-- A webhook audit row belongs to a project even if its repository name
-- is reused. Old rows have no project identity, so only admins see them.
ALTER TABLE audit ADD COLUMN hook_project text NOT NULL DEFAULT '';
