-- Changes made outside Writ, which the control plane learned of from
-- CloudTrail through an environment's queue, in the same portable subset
-- as 0001.

-- A burst of changes: one principal's changes to one service in one
-- account, within the grouping window. One alert goes out for each group.
CREATE TABLE change_groups (
  id            text PRIMARY KEY,
  env           text NOT NULL,
  account       text NOT NULL,
  actor_key     text NOT NULL,          -- the principal's ARN, or its principal id
  actor         text NOT NULL,          -- its short name
  service       text NOT NULL,          -- the event source, such as ec2.amazonaws.com
  first_at      bigint NOT NULL,        -- the event time of the first change
  last_at       bigint NOT NULL,        -- and of the last
  opened_at     bigint NOT NULL,        -- when the first change was recorded
  received_at   bigint NOT NULL,        -- when the last one was
  changes       integer NOT NULL,
  managed       integer NOT NULL,       -- changes that named a resource Writ manages
  -- none: no webhook is configured. pending: the alert waits for the burst
  -- to end. sending: a notifier holds it. sent and failed are final.
  alert         text NOT NULL CHECK (alert IN ('none', 'pending', 'sending', 'sent', 'failed')),
  alert_tries   integer NOT NULL DEFAULT 0,
  alert_next_at bigint NOT NULL DEFAULT 0,
  alert_at      bigint,
  alert_error   text NOT NULL DEFAULT ''
);
CREATE INDEX change_groups_open ON change_groups (env, account, actor_key, service, last_at DESC);
CREATE INDEX change_groups_alert ON change_groups (alert_next_at) WHERE alert = 'pending';
CREATE INDEX change_groups_last ON change_groups (last_at);

-- One CloudTrail event in one environment. The environment and the event
-- id are the key, so a message the queue delivers twice records once, and
-- each environment whose queue receives the event records it.
CREATE TABLE changes (
  env             text NOT NULL,
  event_id        text NOT NULL,
  group_id        text NOT NULL REFERENCES change_groups(id) ON DELETE CASCADE,
  at              bigint NOT NULL,      -- eventTime
  received_at     bigint NOT NULL,
  account         text NOT NULL,
  region          text NOT NULL,
  service         text NOT NULL,
  name            text NOT NULL,        -- the API call, such as AuthorizeSecurityGroupIngress
  actor           text NOT NULL,
  actor_arn       text NOT NULL,
  actor_type      text NOT NULL,        -- IAMUser, AssumedRole, Root, ...
  session         text NOT NULL DEFAULT '',
  source_identity text NOT NULL DEFAULT '',
  source_ip       text NOT NULL DEFAULT '',
  user_agent      text NOT NULL DEFAULT '',
  console         integer NOT NULL DEFAULT 0, -- 1 when made with console credentials
  objects         text NOT NULL,        -- JSON list of the identifiers the event names
  managed         integer NOT NULL,     -- how many resources Writ manages it named
  -- JSON list of {path, value}: request parameters that the provider
  -- schema cleared. withheld says why others are not here.
  params          text NOT NULL DEFAULT '[]',
  withheld        text NOT NULL DEFAULT '',
  PRIMARY KEY (env, event_id)
);
CREATE INDEX changes_env_at ON changes (env, at DESC, event_id);
CREATE INDEX changes_at ON changes (at DESC, event_id, env);
CREATE INDEX changes_group ON changes (group_id);

-- The resources Writ manages that a change named, and what the control
-- plane did to each row: marked it stale, found it already stale, or found
-- that Writ read the resource after the change and left the row alone.
CREATE TABLE change_targets (
  env        text NOT NULL,
  event_id   text NOT NULL,
  stack      text NOT NULL,
  addr       text NOT NULL,
  type       text NOT NULL,             -- the Writ type, such as aws.SecurityGroup
  wire_type  text NOT NULL,
  object     text NOT NULL,             -- the identifier that matched
  outcome    text NOT NULL CHECK (outcome IN ('marked', 'already-stale', 'read-since')),
  PRIMARY KEY (env, event_id, stack, addr),
  FOREIGN KEY (env, event_id) REFERENCES changes(env, event_id) ON DELETE CASCADE
);
CREATE INDEX change_targets_stack ON change_targets (stack, env, event_id);

-- The role sessions the control plane assumed for its runs. A change made
-- with one of them, within its lifetime, is Writ's own: the watcher
-- matches the role, the session name, and the source identity CloudTrail
-- names, never a name's prefix alone. A refresh of the same session moves
-- expires_at.
CREATE TABLE role_sessions (
  role            text NOT NULL,        -- the role's ARN
  session         text NOT NULL,        -- the role session name
  source_identity text NOT NULL,
  first_at        bigint NOT NULL,      -- when the first credentials were issued
  expires_at      bigint NOT NULL,      -- when the last ones expire
  PRIMARY KEY (role, session, source_identity)
);
CREATE INDEX role_sessions_expires ON role_sessions (expires_at);
