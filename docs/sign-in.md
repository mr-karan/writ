# Sign in

People sign in from the terminal with `writ login`, or in the browser at `/ui/`. `writ serve` is the OAuth client for both. A laptop holds only a Writ session token, never a client secret, refresh token, access token, or ID token. A browser holds only its session cookie.

CI jobs authenticate with OIDC tokens instead ([Set up GitLab CI](set-up-gitlab-ci.md)). `writ serve --dev` needs no sign-in.

## How a CLI finds its control plane

Remote commands, `writ login`, and `writ logout` use the first server found:

1. `--server URL`, or `--server dev`. Either implies `--remote`.
2. `WRIT_SERVER`.
3. Your only active `writ login` session. With several, the command stops and lists them.
4. A running `writ serve --dev`, found through `client.json` in the default data directory.

A server URL must be `https://` (or loopback `http://`) with no path, credentials, query, or fragment. For that server, `writ` sends the first credential it has:

1. `WRIT_ID_TOKEN` (a CI OIDC token).
2. The `writ login` session in `~/.config/writ/sessions.json`.
3. The dev token from `client.json`, for dev servers only.

With no server, `writ` exits 2. With no token, it prints how to sign in:

```
$ writ whoami
writ: no control plane: start one with writ serve --dev, or sign in to your team's with writ login URL
run writ help for usage
$ writ runs --remote --server http://127.0.0.1:41093
writ: no token for http://127.0.0.1:41093: sign in with writ login http://127.0.0.1:41093, or in GitLab CI set id_tokens: { WRIT_ID_TOKEN: { aud: http://127.0.0.1:41093 } } on the job
run writ help for usage
```

`writ whoami` shows which server and token `writ` chose. If `writ serve --dev` runs with a custom `--data` directory, set `XDG_DATA_HOME` to its grandparent so the CLI finds `client.json`.

## People: writ login

`writ login` signs in through a `kind = "people"` issuer. `--issuer NAME` picks one when there are several.

| Flow | Used when | Standard |
| --- | --- | --- |
| Device | The issuer publishes `device_authorization_endpoint` | RFC 8628 |
| Browser PKCE on a loopback port | Otherwise, as on GitLab.com | RFC 8252, RFC 7636 |

`--device` or `--browser` forces a flow. Both issue the same `cli` session.

### Sign in with the device flow

```
$ writ login http://127.0.0.1:44701
Signing in to http://127.0.0.1:44701 through issuer sso.
To sign in, open https://127.0.0.1:40329/oauth/device and enter the code HMEZ-SNDH.
The code expires in 5 minutes.
  (the person opens the page, types HMEZ-SNDH, and consents)
Signed in to http://127.0.0.1:44701 as bob (gitlab/user/2002).
Teams: platform
Session ends 2026-11-11T22:36:29Z.
$ writ whoami --remote
http://127.0.0.1:44701 (from your session), session of bob (gitlab/user/2002, issuer sso), ends 2026-11-11T22:36:29Z
teams: platform
prod  *  read approve  -
$ writ logout
Signed out of http://127.0.0.1:44701: session 3VGMLAJ2CWONYGAD3SG56GEP4S of bob ended.
```

Real run against a local test issuer, 2026-11-11. After `writ logout`, the old session token is refused with `auth`.

How it works:

1. `writ login` checks `GET /v1/server`, then calls `POST /v1/auth/device`. `writ serve` starts device auth with the IdP and returns `writ.device/v0`: user code, verification URL, poll interval, expiry, and a 32-byte random handle. The device code stays on the server.
2. `writ login` polls `POST /v1/auth/device/token`. One background goroutine per pending login polls the IdP, honoring `slow_down`.
3. `writ serve` verifies the ID token (issuer, signature, expiry, audience), fetches `groups` from userinfo if configured, resolves teams, stores a `cli` session (SHA-256 of a 32-byte token), and returns `writ.session/v0` once.
4. `writ` saves the session in `$XDG_CONFIG_HOME/writ/sessions.json` (`~/.config/writ/sessions.json`), mode `0600` in a `0700` directory. It refuses symlinks and loose permissions. Signing in again revokes your previous session on that server.

Pending login handles live in memory and are lost on server restart.

### Sign in through a browser

GitLab.com's discovery document omits `device_authorization_endpoint` (checked 2026-11-16), so `writ login` opens a browser on the same machine and receives the OAuth callback on an ephemeral loopback port:

```
$ writ login http://127.0.0.1:35309
Signing in to http://127.0.0.1:35309 through issuer sso.
To sign in, open https://127.0.0.1:36093/oauth/authorize?client_id=writ-e2e&code_challenge=QqueYeHu6tMmOgOABEnQeLVDZ4E2tW41WqKkByO5AdM&code_challenge_method=S256&nonce=BV_F4WYQLxLGVMYFi9phIfQaGZKgk6l7-QCRw0TZzEU&redirect_uri=http%3A%2F%2F127.0.0.1%3A36443%2Fcallback&response_type=code&scope=openid+profile+email&state=BGX7AJ5XD2T6KK7AUC6OMKYHR2 in a browser on this computer.
The sign-in expires in 10 minutes.
Signed in to http://127.0.0.1:35309 as bob (gitlab/user/2002).
Teams: acme
Session ends 2026-11-16T20:39:54Z.
```

Real run against a local test issuer, 2026-11-16.

How it works:

1. `writ login` binds `127.0.0.1:0`, makes `state` and a PKCE verifier, and posts the `http://127.0.0.1:PORT/callback` redirect URI and S256 challenge to `POST /v1/auth/browser`.
2. `writ serve` checks the exact loopback callback format and the 43-character S256 challenge, makes a nonce, stores a 10-minute in-memory handle, and returns the IdP authorization URL (`writ.browser/v0`).
3. The CLI prints the URL and, on a TTY, opens it with `xdg-open`, `open`, or `rundll32`. It refuses non-`https://` URLs.
4. The local listener accepts one request and closes, or times out after 10 minutes. It rejects non-loopback peers, a `Host` header that does not match (against DNS rebinding), wrong paths, and a mismatched `state`.
5. `writ login` sends the handle, code, state, and verifier to `POST /v1/auth/browser/token`, which consumes the handle. `writ serve` verifies state and PKCE, exchanges the code with its client secret, validates the ID token and nonce, and issues a `cli` session.

Over SSH, forward the loopback port, or pass `--device` if your IdP supports device auth. A `writ serve` too old for browser sign-in has no `POST /v1/auth/browser`; `writ login --browser` then says so and suggests `--device`.

### Sign-ins that wait

Pending, unauthenticated logins in server memory are bounded:

- At most 1,000 pending device logins and 1,000 pending browser logins in total.
- At most 10 pending logins of both kinds per client address (IPv6 `/64`). Beyond that: `429`.
- If the audit write fails, the request gets `503` and the slot is freed.
- All login routes count toward `limits.requests_per_second`.

**Limits:** many addresses together can still fill the 1,000. Every new sign-in then gets `429` until theirs run out: 10 minutes for a browser sign-in, or the device code's life. Behind a reverse proxy not listed in `trusted_proxies`, all users share one address and its 10-slot cap.

### Sessions

- A session expires after `session_ttl` (default `12h`). There is no refresh.
- Teams are fixed for the session's life. A person removed from an IdP group keeps the team until the session ends.
- A `cli` session works as a bearer token on `/v1/*` (`via = cli session`) and cannot open `/ui/`. A browser session cannot be a `/v1/*` bearer token.
- `writ logout` calls `DELETE /v1/auth/session` and deletes the local entry. If the server is unreachable, it still deletes the local entry and fails, because the session stays live on the server until it ends.
- Any principal with `admin` on any grant can list sessions (`GET /v1/sessions`, filter `?person=`) and revoke one at once (`DELETE /v1/sessions/{id}`).
- Session tokens never appear in logs, audit rows, UI pages, or later API responses.
- The CLI never follows HTTP redirects from `writ serve`, so `Authorization` headers cannot leak across schemes or ports.

### Set up writ login

- **Device flow:** the issuer must publish `device_authorization_endpoint` in `/.well-known/openid-configuration`, and the OAuth app must enable device auth.
- **Browser flow:** register `http://127.0.0.1/callback` next to `https://URL/ui/callback` in the confidential OAuth app. The IdP must allow any loopback port, per RFC 8252 §7.3. Doorkeeper 5.1+ does, but live GitLab has not been tested end to end.

## The control UI

`writ serve` renders server-side HTML under `/ui/`, and `GET /` redirects there. Each page calls the same `/v1` handlers in-process with your browser session, so it enforces the same grants and writes the same audit rows (`via = browser session`), plus one audit row for the page request. Every page footer shows the matching API paths, CLI commands, and a `?as=json` link.

| Page | Documents | Contents |
| --- | --- | --- |
| `/ui/` | `writ.overview/v0` | Counts (pending approvals, active applies, 24h failures, drifted stacks), alerts (drift, failed checks, outside writes, failed runs, expiring plans, failed polls), active and pending plans, and up to 200 OpenTofu stacks |
| `/ui/stack?stack=&env=` | `writ.state-index/v0`, `writ.drift/v0`, `writ.remote-runs/v0`, `writ.remote-plans/v0` | State index resources (never raw state values), latest drift check, outside-write markers, runs, and plans |
| `/ui/runs`, `/ui/runs/{run}` | `writ.remote-runs/v0`, `writ.remote-run/v0` | Apply history, per-resource masked apply events or failure diagnostics |
| `/ui/plans`, `/ui/plans/{id}` | `writ.remote-plans/v0`, `writ.remote-plan/v0` | Masked plan diff by module and address, risk classes, sensitive markers, drift, outputs, policy results, and the Approve button |
| `/ui/drift/{ref}` | `writ.drift-check/v0` | One drift check: outside and code changes, commit, timing, or an error with the `writ state adopt` command |
| `/ui/approvals` | `writ.remote-plans/v0` | Pending plans waiting on others or on you |
| `/ui/access`, `/ui/whoami` | `writ.config/v0`, `writ.whoami/v0` | Configured teams and grants, and your effective permissions |
| `/ui/audit` | `writ.audit/v0` | Filterable audit log ([Read the audit log](audit-log.md#4-read-the-audit-log-in-the-control-ui)) |
| `/ui/settings` | `writ.server/v0`, `writ.health/v0`, `writ.config/v0` | Server status, forges, notifier delivery counts, and browser theme |
| `/ui/start` | `writ.server/v0`, `writ.repos/v0`, `writ.repo/v0`, `writ.remote-plans/v0` | Onboarding checklist and next step |
| `/ui/repos`, `/ui/repos/{name}`, `/ui/repos/new` | `writ.repos/v0`, `writ.repo/v0` | Registered repositories, poll and webhook status, stacks, and the Add repository form |

**Limits:** active run pages refresh every 5 seconds instead of streaming live events. Browser sign-in has been tested against a local test issuer, not a live GitLab instance.

### Signing in

1. Register `https://URL/ui/callback` with scopes `openid profile email` on your confidential OAuth app.
2. An unauthenticated `/ui/*` request redirects (`303`) to `/ui/login?next=PATH`. `next` must be under `/ui/`.
3. `/ui/login/start` stores `state`, `nonce`, and a PKCE verifier in a 10-minute `HttpOnly` cookie and redirects to the IdP with an S256 challenge. Writ sends the challenge even when the discovery document does not list S256, and logs a warning at start.
4. `/ui/callback` checks `state` in constant time, clears the cookie, exchanges the code with the verifier and client secret, validates the ID token and nonce, resolves teams, and stores a `browser` session.
5. `POST /ui/logout` ends the server session. It does not sign you out of the IdP.

If a page cannot write its own audit row, it answers `503`, sets no cookie, and redirects nowhere. A sign-out that cannot end its session shows that page too, and the cookie and session stay.

### What every page sends

- Session cookies are `HttpOnly` and `SameSite=Lax`, and on `https://` URLs also `Secure` with the `__Host-` prefix.
- Forms are protected by cross-origin request checks (`Sec-Fetch-Site` and `Origin`).
- Every `/ui/` response sets a strict CSP: no inline scripts or styles, no third-party origins.

```
Cache-Control: no-store
Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'
Content-Type: text/html; charset=utf-8
Referrer-Policy: no-referrer
X-Content-Type-Options: nosniff
Date: Mon, 16 Nov 2026 06:22:31 GMT
Transfer-Encoding: chunked
```

Headers from `/ui/whoami`, 2026-11-16.

Pages work with JavaScript disabled. JavaScript adds local-timezone tooltips, relative timestamps, menu toggles, and copy buttons. Pages use system fonts, load no external assets, and support light and dark themes.

## Troubleshooting

| Problem | Fix |
| --- | --- |
| Redirect URI mismatch in GitLab | Register `https://writ.example.com/ui/callback` and `http://127.0.0.1/callback` on separate lines. |
| `writ login` fails over SSH | Forward the loopback port, or use `--device` if your IdP supports device auth. |
| `429` when starting sign-in behind a reverse proxy | Add the proxy's CIDR to `trusted_proxies`, so users do not share one address's limit of 10 pending logins. |
| `sessions.json` refused | It must be a regular file you own, `0600` in a `0700` directory. Delete it and run `writ login` again. |
| An offboarded user still has access | Revoke their sessions at once: `GET /v1/sessions?person=PERSON_ID`, then `DELETE /v1/sessions/{id}`. |

## Next steps

- [Set up GitLab CI](set-up-gitlab-ci.md): OIDC auth for CI jobs.
- [API: Token checks](api.md#token-checks): bearer and OIDC validation rules.
- [Plan and apply](apply.md): plan, approve, and apply.
