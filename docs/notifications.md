# Get notified

Send a Slack message or a JSON webhook when a stack drifts, a plan or apply fails, an apply succeeds, a state is written outside Writ, or a plan needs approval. Add one `notify` block per destination to `/etc/writ/serve`.

Notifications never include secrets, plan diffs, diagnostic text, or credentials. They carry only Writ's fixed wording, stack and env names, counts, error classes (such as `provider` or `usage`), and a link to the control UI page.

## Send to Slack

Create a Slack [incoming webhook](https://api.slack.com/messaging/webhooks) and store its URL in a file readable only by `writ serve`:

```sh
umask 077
printf '%s\n' 'https://hooks.slack.com/services/YOUR/WEBHOOK/PATH' > /etc/writ/slack-ops.url
chown writ /etc/writ/slack-ops.url
```

Add a `notify` block to `/etc/writ/serve`:

```hcl
notify "ops-slack" {
  kind     = "slack"
  url_file = "/etc/writ/slack-ops.url"
  events   = ["drift", "apply-failed", "approval-needed"]
  envs     = ["prod"]
}
```

Writ escapes `<`, `>`, and `&`, collapses newlines to spaces, and wraps display names in inline code, so untrusted names cannot trigger mentions or inject formatting:

```json
{"text":"[drift] app@prod: drifted: 1 change outside Writ, 2 changes in code","blocks":[{"type":"section","text":{"type":"mrkdwn","text":"*[drift]* `app@prod`: drifted: 1 change outside Writ, 2 changes in code\nStarted by: drift check"}}]}
```

Real run, 2026-11-20. It has no control UI link because the control UI was disabled.

## Send to your own webhook

A `webhook` notifier sends a JSON `POST` to `url_file`. Signing is optional: set `secret_file` (at least 32 random bytes, such as `openssl rand -hex 32`) to sign requests with HMAC-SHA256:

```hcl
notify "pager" {
  kind        = "webhook"
  url_file    = "/etc/writ/pager.url"
  secret_file = "/etc/writ/pager.secret"
  events      = ["apply-failed", "outside-write"]
  stacks      = ["payments/**"]
}
```

### The payload

Example `applied` event from a CI job (excerpt of a real run, 2026-11-20):

```
POST /hooks/WEBHOOK_SECRET_PATH
X-Writ-Event: applied
X-Writ-Delivery: 7I6SLPEAKTWGIFMADEROJEITY4
X-Writ-Timestamp: 1795160293
X-Writ-Signature-256: sha256=ae0888cb67b721f4e0ae95b08347ff29938245b3e87c5f485a13d2b4ad4fd00b

{"format":"writ.notification/v0","id":"7I6SLPEAKTWGIFMADEROJEITY4","event":"applied","stack":"app","env":"prod","status":"applied","summary":"applied: 3 added, 0 changed, 0 destroyed", ...}
```

Every field is in [Notification payload](json-output.md#notification-payload-writnotificationv0).

### Verify the signature

- With `secret_file` set, Writ adds `X-Writ-Signature-256`. Without it, deliveries are unsigned.
- The header is `sha256=` plus the lowercase hex HMAC-SHA256 of `<X-Writ-Timestamp>.<raw body>`. Each retry updates `X-Writ-Timestamp` and the signature.
- Verify over the raw bytes, in constant time, before parsing JSON, and reject timestamps more than 5 minutes off:

```go
func verify(secret []byte, r *http.Request, body []byte) bool {
	ts := r.Header.Get("X-Writ-Timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(sec, 0)).Abs() > 5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(r.Header.Get("X-Writ-Signature-256")))
}
```

## Events

| Event | Trigger |
| --- | --- |
| `drift` | Stack transitions from clean/unchecked to drifted, or from drifted back to clean. Set `drift_mode = "every-check"` to alert on every drifted check (failed checks don't flip state, and applies between two drifted checks don't reset the default `on-change` mode). |
| `plan-failed` | Plan ends `failed` or `refused` (summary includes first error class). |
| `apply-failed` | Apply ends `failed` (summary includes first error class). |
| `applied` | Apply succeeds (includes add/change/destroy/import counts). |
| `outside-write` | Plan, adoption, or state-index refresh reads an S3 serial Writ didn't write (reported once per serial). |
| `approval-needed` | Plan ends `pending` and requires at least one approval. |

Filter notifiers with `envs` (environment names or `"*"`) and `stacks` ([stack patterns](config.md#stack-patterns)); omit either to match all.

## Check the setup

`writ serve --check` validates every `url_file` and `secret_file` and prints stored delivery counts. Add `--notify-test` to send a `test` event to each notifier and fail if any delivery fails:

```
notify: ops (webhook) hears applied drift in prod for payments/**, drift on-change; 0 delivered, 0 failed
notify: ops: test message sent
```

Real run, 2026-11-20. If `--check` cannot read delivery counts (unreachable database, missing permissions, or unmigrated schema), it prints a notice and does not fail.

In the control UI, **Settings > Notifications** (and `GET /v1/config` under `notifiers`) shows each notifier's kind, events, delivered/failed counts, last delivery, and last error, never URLs, secrets, or file paths.

## Troubleshooting

| Problem | Fix |
| --- | --- |
| A delivery fails with `tls:` or `dns:` | The certificate is untrusted, expired, or mismatched, the HTTPS URL answers in plaintext, or the host does not exist. TLS errors and `dns: no such host` fail at once, without retries; DNS timeouts and temporary lookup failures are retried. |
| A delivery fails on a redirect | Redirects (`3xx`) fail. Point `url_file` at the final URL. |
| Changed `url_file` or `secret_file` has no effect | Both are read once at startup. Restart `writ serve`. |
| Events go missing under load | A full 128-event queue drops new events, counted as failed. Check **Settings > Notifications**. |
| Deliveries ignore the env `egress` list | Notifications come from `writ serve`, not runs, so env `egress` lists do not apply and `HTTPS_PROXY` is honored. |

Retries, timeouts, and size limits are in [Notification delivery](config.md#notification-delivery).
