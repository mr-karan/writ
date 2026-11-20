-- When a pinned repository's forge webhook last reached writ serve and
-- passed its check, NULL before the first one. The delivery's body is
-- never kept.
ALTER TABLE repos ADD COLUMN hook_at bigint;
