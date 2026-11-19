-- The destinations an OpenTofu plan's run, and an apply's run, asked
-- writ's egress proxy for, as writ.network/v1 with each host masked.
-- NULL when the run had no egress proxy: egress_mode was off, or OpenTofu
-- never ran.
ALTER TABLE plans ADD COLUMN network text;
ALTER TABLE runs ADD COLUMN network text;
