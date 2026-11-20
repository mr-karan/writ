-- Notification delivery counters and last outcome per configured notifier.
-- Holds no webhook URL, signing secret, or upstream response body.
CREATE TABLE notification_status (
  name           text PRIMARY KEY,
  delivered      bigint NOT NULL DEFAULT 0,
  failed         bigint NOT NULL DEFAULT 0,
  last_event     text NOT NULL DEFAULT '',
  last_ok_at     bigint NOT NULL DEFAULT 0,
  last_error_at  bigint NOT NULL DEFAULT 0,
  last_error     text NOT NULL DEFAULT ''
);
