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
{
  "text": "[drift] app@prod: drifted: 1 change outside Writ, 2 changes in code",
  "blocks": [
    {
      "type": "section",
      "text": {
        "type": "mrkdwn",
        "text": "*[drift]* `app@prod`: drifted: 1 change outside Writ, 2 changes in code\nStarted by: drift check"
      }
    }
  ]
}
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

Example `applied` event from a CI job:

```
POST /hooks/WEBHOOK_SECRET_PATH
User-Agent: writ/test
X-Writ-Event: applied
X-Writ-Delivery: 7I6SLPEAKTWGIFMADEROJEITY4
X-Writ-Timestamp: 1795160293
X-Writ-Signature-256: sha256=ae0888cb67b721f4e0ae95b08347ff29938245b3e87c5f485a13d2b4ad4fd00b

{
  "format": "writ.notification/v0",
  "id": "7I6SLPEAKTWGIFMADEROJEITY4",
  "event": "applied",
  "at": "2026-11-20T07:38:12.752696503Z",
  "server": "http://127.0.0.1:8080",
  "stack": "app",
  "env": "prod",
  "status": "applied",
  "summary": "applied: 3 added, 0 changed, 0 destroyed",
  "started_by": {
    "kind": "ci",
    "display": "gitlab acme/infra #1849213 (bob)",
    "person": "gitlab/user/2002"
  },
  "plan": "09868de5c43e904f15a3e7b658530af2",
  "run": "OUFH4WVRRERDOKUZK7PKFQF3UQ",
  "counts": {
    "added": 3
  }
}
```

Output of a real run, 2026-11-20 (body indented; `url` omitted because the control UI was disabled).

| Field | Meaning |
| --- | --- |
| `format` | `writ.notification/v0`. |
| `id` | Stable delivery ID across retries (use for deduplication; trust the signed body over `X-Writ-Delivery`). |
| `event` | One of the [events](#events) (trust the signed body over `X-Writ-Event`). |
| `at` | Event timestamp (UTC). |
| `server` | Server config's `url`. |
| `stack`, `env` | Target `stack@env`. |
| `status` | `pending`, `failed`, `refused`, `applied`, `drifted`, `clean`, or `outside-write`. |
| `summary` | One-line Writ summary. |
| `started_by` | `kind` (`person`, `ci`, `drift`, or `outside`), plus `display` and `person`. |
| `plan`, `run` | Plan ID (or ref if failed before save) and run ID. |
| `counts` | Non-zero event counts (`changes`/`need` for `approval-needed`; `added`/`changed`/`destroyed`/`imported` for `applied`; `outside`/`code` for `drift`; `serial` for `outside-write`). |
| `url` | Control UI link (omitted when the control UI is disabled). |

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

## How delivery works

Each notifier has its own 128-event queue and background sender, so slow receivers never block plans or applies:

- **Retries:** Timeouts, connection errors, temporary DNS failures, `429`, and `5xx` retry at 1s, 4s, 16s, 1m, and 3m. A `Retry-After` header on `429` extends the delay up to 5 minutes (and paces the next queued event if retries exhaust). Other 4xx statuses fail immediately.
- **Immediate operator errors:** Untrusted/expired/mismatched TLS certificates, plaintext HTTP on an HTTPS URL, and nonexistent hostnames fail immediately with explicit `tls:` or `dns:` errors.
- **Bounds:** 10-second per-attempt timeout, 16 KiB max request body, 4 KiB max response read (discarded).
- **Target rules:** URLs must be `https://` (or loopback `http://` for tests) with no embedded credentials. Redirects (`3xx`) fail.
- **Queue overflow:** When all 128 slots are full, new events drop with a log warning and count as failed on the next flush.
- **Restarts:** `url_file` and `secret_file` are read once at startup; restart `writ serve` after changing them. Unsent queued events don't persist across restarts.

`writ serve --check` verifies that `provider_uids` can't read `url_file` or `secret_file`. Notifications originate from `writ serve` itself (not OpenTofu runs), so environment `egress` allow-lists don't apply and `HTTPS_PROXY` is honored.
