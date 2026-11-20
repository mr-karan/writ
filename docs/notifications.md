# Get notified

`writ serve` can tell a Slack channel or your own webhook when a stack drifts, a plan or an apply fails, an apply lands, a state is written outside Writ, or a plan waits for approval. You add a `notify` block to the server config for each destination. Each message says what happened, on which `stack@env`, who or what started it, and links to the page in the control UI.

A message never contains a secret value, a plan body, a diagnostic's text, or a credential. Its summary is built only from Writ's own words, names, counts, and the diagnostic class, such as `provider` or `usage`. To see why a plan failed, follow the link.

## Send to Slack

Create an [incoming webhook](https://api.slack.com/messaging/webhooks) for the channel, and put its URL in a file only `writ serve`'s user can read:

```sh
umask 077
printf '%s\n' 'https://hooks.slack.com/services/YOUR/WEBHOOK/PATH' > /etc/writ/slack-ops.url
chown writ /etc/writ/slack-ops.url
```

Then add a `notify` block to the server config:

```hcl
notify "ops-slack" {
  kind     = "slack"
  url_file = "/etc/writ/slack-ops.url"
  events   = ["drift", "apply-failed", "approval-needed"]
  envs     = ["prod"]
}
```

A Slack message has one line of text: the event, the `stack@env`, the summary, who started it, and a **View in Writ** link. Writ escapes `<`, `>`, and `&` in every field, turns line breaks into spaces, and puts the display name in a code span, so neither a stack name nor a display name a person chose can mention a channel, add a link, or start a line of its own. This message came from a drift check:

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

Output of `TestNotifyEvents` in `internal/server/notify_test.go`, 2026-11-20. The test server has no control UI, so the message has no link.

## Send to your own webhook

A `webhook` notifier sends each event as a JSON `POST` to the URL in its `url_file`. Add `secret_file` to sign each request with HMAC-SHA256, so your receiver can check that the request came from `writ serve`:

```hcl
notify "pager" {
  kind        = "webhook"
  url_file    = "/etc/writ/pager.url"
  secret_file = "/etc/writ/pager.secret"
  events      = ["apply-failed", "outside-write"]
  stacks      = ["payments/**"]
}
```

Use a random secret of at least 32 bytes, such as the output of `openssl rand -hex 32`.

### The payload

This request came from an apply that a CI job started:

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

Output of `TestNotifyEvents`, 2026-11-20, with the body indented. The test server has no control UI, so the payload has no `url`.

| Field | What it is |
| --- | --- |
| `format` | `writ.notification/v0`. |
| `id` | The delivery's id. A retry sends the same id, so use it to drop duplicates. `X-Writ-Delivery` has it too, but the signature doesn't cover headers, so trust the body's `id`. |
| `event` | One of the [events](#events). `X-Writ-Event` has it too, but the signature doesn't cover headers, so trust the body's `event`. |
| `at` | When it happened, in UTC. |
| `server` | The server config's `url`. |
| `stack`, `env` | The `stack@env` it happened on. |
| `status` | The plan's, run's, or drift check's status: `pending`, `failed`, `refused`, `applied`, `drifted`, `clean`, or `outside-write`. |
| `summary` | One line in Writ's words. |
| `started_by` | Who or what started it. `kind` is `person`, `ci`, `drift` for a drift check, or `outside` for a write outside Writ. `display` and `person` are the token's display name and person id. |
| `plan`, `run` | The plan's id, or its ref when it failed before Writ saved it, and the run's id. |
| `counts` | Numbers for the event: `changes` and `need` (approvals) for `approval-needed`; `added`, `changed`, `destroyed`, and `imported` for `applied`; `outside` and `code` for `drift`; `serial` for `outside-write`. A zero count is left out. |
| `url` | The page in the control UI: the plan, the run, or the stack's drift view. Left out when the control UI is off. |

### Verify the signature

With `secret_file` set, `X-Writ-Signature-256` is `sha256=` and the lowercase hex HMAC-SHA256 of the timestamp, a `.`, and the raw body, keyed with the secret. `X-Writ-Timestamp` is the Unix time of this attempt, so each retry has a new timestamp and signature.

Check the signature over the raw bytes, before you parse the JSON, compare in constant time, and reject a timestamp more than five minutes from your clock. In Go:

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

| Event | When it's sent |
| --- | --- |
| `drift` | A drift check finds drift on a `stack@env` that was clean or never checked, or finds it clean again after drift. With `drift_mode = "every-check"`, every check that finds drift sends one too. A failed drift check doesn't count as clean or drifted. An apply between two checks doesn't reset this: drift found again after an apply, with no clean check between, sends nothing under the default mode. Use `every-check` to hear about it. |
| `plan-failed` | A plan ends `failed` or `refused`. The summary has the first error's class. |
| `apply-failed` | An apply ends `failed`. The summary has the first error's class. |
| `applied` | An apply ends `applied`, with what it added, changed, destroyed, and imported. |
| `outside-write` | A plan, an adoption, or a refresh of the state index reads a serial of a state on S3 that no apply of `writ serve` wrote. Each serial is reported once. See [Drift checks](drift-checks.md). |
| `approval-needed` | A plan ends `pending` and needs at least one approval. |

`envs` and `stacks` narrow a notifier the way they narrow a grant: `envs` lists environment names or `"*"`, and `stacks` lists [stack patterns](serve.md#stack-patterns). Leave either out to hear about every environment or every stack.

## Check the setup

`writ serve --check` lists each notifier with its events and the delivery counts the control database keeps. It fails when a `url_file` or `secret_file` can't be read or doesn't hold a URL `writ serve` will send to. It sends nothing unless you add `--notify-test`, which sends one test message, event `test`, to each notifier and fails if one doesn't arrive:

```
notify: ops (webhook) hears applied drift in prod for payments/**, drift on-change; 0 delivered, 0 failed
notify: ops: test message sent
```

The notify lines of `writ serve --check --notify-test` in `TestServeCheckNotify`, `internal/cli/servecheck_notify_test.go`, 2026-11-20. When `--check` can't reach the control database, it says so on a `notify: delivery counts not read` line and doesn't fail.

The **Notifications** card on the control UI's Settings page shows each notifier's kind, events, how many messages it delivered and failed, its last delivery, and its last error. `GET /v1/config` returns the same under `notifiers`. Neither shows a URL, a secret, or a file path.

## How delivery works

Delivery never holds up a plan or an apply. Each notifier has its own queue of 128 events and one sender, so a slow receiver delays only its own messages.

- **Retries.** A timeout, a connection failure, a `429`, or a `5xx` is retried after 1 s, 4 s, 16 s, 1 min, and 3 min. After the last retry, `writ serve` gives up, logs a warning, and counts the event as failed. Any other status fails at once.
- **Bounds.** Each attempt has 10 seconds. A body is at most 16 KiB, and Writ reads at most 4 KiB of the answer, which it keeps nothing of.
- **Where it sends.** Only to the URL in `url_file`. It must be `https://`, or `http://` on a loopback address for tests, with no user name or password in it. Writ follows no redirect: a `3xx` answer is a failure.
- **A full queue.** When a notifier's queue is full, the new event is dropped for that notifier and logged. The next delivery records how many were dropped as failed.
- **Restarts.** `writ serve` reads each `url_file` and `secret_file` once, when it starts. Restart it after you change one. Events still queued when it stops are not sent.

The isolation check of `writ serve --check` also checks that a provider uid can't read a `url_file` or a `secret_file`, as it does for the other secret files. A notifier's outbound requests come from `writ serve` itself, not from an OpenTofu run, so the environment's `egress` list doesn't apply to them. `writ serve` honors `HTTPS_PROXY` for them.
