# Sign in

People sign in from the terminal with `writ login` or in the browser through `/ui/`. `writ serve` acts as the OAuth client for both, so laptops hold only a Writ session token (no client secret, refresh token, access token, or ID token).

`writ login` uses the OAuth device flow when the identity provider publishes `device_authorization_endpoint`, and falls back to the PKCE browser loopback flow otherwise (as on GitLab.com). CI jobs authenticate directly with OIDC tokens ([Set up GitLab CI](set-up-gitlab-ci.md)); `writ serve --dev` needs no sign-in.

## How a CLI finds its control plane

Remote commands, `writ login`, and `writ logout` pick the first server configured:

1. `--server URL` (or `--server dev`; implies `--remote`).
2. `WRIT_SERVER`.
3. Your sole active `writ login` session (if multiple exist, the command stops and lists them).
4. A running `writ serve --dev` found via `client.json` in the default data directory.

Server URLs must be `https://` (or loopback `http://`) with no path, credentials, query, or fragment. For the selected server, `writ` sends the first available credential:

1. `WRIT_ID_TOKEN` (CI OIDC token).
2. The `writ login` session in `~/.config/writ/sessions.json`.
3. The dev token from `client.json` (dev servers only).

Missing server exits 2; missing token prints sign-in instructions:

```
$ writ whoami
writ: no control plane: start one with writ serve --dev, or sign in to your team's with writ login URL
run writ help for usage
$ writ runs --remote --server http://127.0.0.1:41093
writ: no token for http://127.0.0.1:41093: sign in with writ login http://127.0.0.1:41093, or in GitLab CI set id_tokens: { WRIT_ID_TOKEN: { aud: http://127.0.0.1:41093 } } on the job
run writ help for usage
```

Output of a real run, 2026-11-16. Run `writ whoami` to inspect which server and token `writ` selected. If `writ serve --dev` used a custom `--data` directory, set `XDG_DATA_HOME` to its grandparent so CLI discovery finds `client.json`.

## People: writ login

`writ login` uses `golang.org/x/oauth2` against a `kind = "people"` issuer:

- **Device flow (RFC 8628):** used when the issuer publishes `device_authorization_endpoint`.
- **Browser PKCE flow (RFC 8252 / RFC 7636):** used otherwise ([Sign in through a browser](#sign-in-through-a-browser)).

Pass `--device` or `--browser` to force a flow. Both issue the same `cli` session.

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

Output of an end-to-end run against a test issuer, 2026-11-11. After `writ logout`, the old session token is immediately refused with `auth`.

### What happens when you sign in

1. `writ login` checks `GET /v1/server`, then calls `POST /v1/auth/device`. `writ serve` starts device auth with the IdP and returns `writ.device/v0` (user code, verification URL, poll interval, expiry, and a 32-byte random handle; the device code stays on the server).
2. `writ login` polls `POST /v1/auth/device/token`. One background goroutine per pending login polls the IdP (honoring `slow_down`).
3. On completion, `writ serve` verifies the ID token (issuer, signature, expiry, audience), fetches `groups` from userinfo if configured, resolves teams, stores a `cli` session (SHA-256 of a 32-byte token), and returns `writ.session/v0` once.
4. `writ` saves the session in `$XDG_CONFIG_HOME/writ/sessions.json` (`~/.config/writ/sessions.json`), mode `0600` in a `0700` directory, refusing symlinks or loose permissions. Signing in again revokes the previous session for that server.

Pending login handles live in memory and drop on server restart.

### Sign in through a browser

Because GitLab.com's discovery document omits `device_authorization_endpoint` (checked 2026-11-16), `writ login` opens a browser on the same machine and receives the OAuth callback on an ephemeral loopback port:

```
$ writ login http://127.0.0.1:35309
Signing in to http://127.0.0.1:35309 through issuer sso.
To sign in, open https://127.0.0.1:36093/oauth/authorize?client_id=writ-e2e&code_challenge=QqueYeHu6tMmOgOABEnQeLVDZ4E2tW41WqKkByO5AdM&code_challenge_method=S256&nonce=BV_F4WYQLxLGVMYFi9phIfQaGZKgk6l7-QCRw0TZzEU&redirect_uri=http%3A%2F%2F127.0.0.1%3A36443%2Fcallback&response_type=code&scope=openid+profile+email&state=BGX7AJ5XD2T6KK7AUC6OMKYHR2 in a browser on this computer.
The sign-in expires in 10 minutes.
Signed in to http://127.0.0.1:35309 as bob (gitlab/user/2002).
Teams: acme
Session ends 2026-11-16T20:39:54Z.
```

Output of `TestTofuApproveApply` in `e2e/`, 2026-11-16, with a test issuer and scripted browser.

1. `writ login` binds `127.0.0.1:0`, generates `state` and a PKCE verifier, and posts the `http://127.0.0.1:PORT/callback` redirect URI and S256 challenge to `POST /v1/auth/browser`.
2. `writ serve` validates the exact loopback callback format and 43-char S256 challenge, creates a nonce, stores a 10-minute in-memory handle, and returns the IdP authorization URL (`writ.browser/v0`).
3. The CLI prints the URL (and opens it via `xdg-open`, `open`, or `rundll32` on a TTY; refuses non-`https://` URLs).
4. The local listener accepts one request (rejecting non-loopback peers, mismatched `Host` headers against DNS rebinding, wrong paths, or mismatched `state`) and closes, or times out after 10 minutes.
5. `writ login` submits the handle, code, state, and verifier to `POST /v1/auth/browser/token` (consuming the handle). `writ serve` verifies state and PKCE, exchanges the code with its client secret, validates the ID token and nonce, and issues a `cli` session.

Over SSH, forward the loopback port or pass `--device` if your IdP supports device auth.

### Sign-ins that wait

Unauthenticated pending logins in server memory are bounded:

- At most 1,000 pending device logins and 1,000 pending browser logins globally.
- At most 10 pending logins combined per client address (IPv6 `/64`), returning `429` beyond that.
- Audit write failure returns `503` and frees the slot.
- All login routes count toward `limits.requests_per_second`.

**Limits:** Behind a reverse proxy not listed in `trusted_proxies`, all users share one address and the 10-slot per-address cap.

### Sessions

- Sessions expire after `session_ttl` (default `12h`) with no refresh; teams stay fixed until re-login.
- `cli` sessions work as bearer tokens on `/v1/*` (`via = cli session`) and can't open `/ui/`. Browser sessions can't be used as `/v1/*` bearer tokens.
- `writ logout` calls `DELETE /v1/auth/session` and deletes the local entry (deleting locally even if the server is unreachable, with an error).
- Any principal with `admin` on any grant can list sessions (`GET /v1/sessions`, filter `?person=`) and revoke them immediately (`DELETE /v1/sessions/{id}`).
- Session tokens never appear in logs, audit rows, UI pages, or subsequent API responses.
- The CLI never follows HTTP redirects from `writ serve` so `Authorization` headers can't leak across schemes or ports.

### Set up writ login

`writ login` works with any `kind = "people"` issuer (`--issuer NAME` chooses among multiple).

- **Device flow:** Issuer must publish `device_authorization_endpoint` in `/.well-known/openid-configuration` and enable device auth on the OAuth app.
- **Browser flow:** Register `http://127.0.0.1/callback` alongside `https://URL/ui/callback` in the confidential OAuth app. The IdP must allow arbitrary loopback ports per RFC 8252 §7.3 (Doorkeeper 5.1+ supports this, though live GitLab hasn't been tested end-to-end yet).

## The control UI

`writ serve` renders server-side HTML pages under `/ui/` (`GET /` redirects to `/ui/`). Each page calls the same `/v1` handlers in-process using your browser session, enforcing identical grants and writing audit rows (`via = browser session`), plus an audit row for the page request itself. Every page footer shows the matching API paths, CLI commands, and a `?as=json` link.

| Page | Documents | Contents |
| --- | --- | --- |
| `/ui/` | `writ.overview/v0` | Counts (pending approvals, active applies, 24h failures, drifted stacks), alerts (drift, failed checks, outside writes, failed runs, expiring plans, failed polls), active/pending plans, and up to 200 OpenTofu stacks |
| `/ui/stack?stack=&env=` | `writ.state-index/v0`, `writ.drift/v0`, `writ.remote-runs/v0`, `writ.remote-plans/v0` | State index resources (never raw state values), latest drift check, outside-write markers, runs, and plans |
| `/ui/runs`, `/ui/runs/{run}` | `writ.remote-runs/v0`, `writ.remote-run/v0` | Apply history and per-resource masked apply events or failure diagnostics |
| `/ui/plans`, `/ui/plans/{id}` | `writ.remote-plans/v0`, `writ.remote-plan/v0` | Masked plan diff by module/address, risk classes, sensitive markers, drift, outputs, policy results, and Approve button |
| `/ui/drift/{ref}` | `writ.drift-check/v0` | Single drift check: outside vs code changes, commit, timing, or error with `writ state adopt` command |
| `/ui/approvals` | `writ.remote-plans/v0` | Pending plans waiting on others or on you |
| `/ui/access`, `/ui/whoami` | `writ.config/v0`, `writ.whoami/v0` | Configured teams/grants and your effective permissions |
| `/ui/audit` | `writ.audit/v0` | Filterable audit log ([Read the audit log](audit-log.md#4-read-the-audit-log-in-the-control-ui)) |
| `/ui/settings` | `writ.server/v0`, `writ.health/v0`, `writ.config/v0` | Server status, forges, notifier delivery counts, and browser theme |
| `/ui/start` | `writ.server/v0`, `writ.repos/v0`, `writ.repo/v0`, `writ.remote-plans/v0` | Onboarding checklist and next step |
| `/ui/repos`, `/ui/repos/{name}`, `/ui/repos/new` | `writ.repos/v0`, `writ.repo/v0` | Registered repositories, poll/webhook status, stacks, and Add repository form |

**Limits:** Active run pages refresh every 5 seconds rather than streaming live events. Browser sign-in has been tested against a local test issuer, not a live GitLab instance.

### Signing in

1. Register `https://URL/ui/callback` with scopes `openid profile email` on your confidential OAuth app.
2. Unauthenticated `/ui/*` requests redirect (`303`) to `/ui/login?next=PATH` (`next` is restricted to `/ui/`).
3. `/ui/login/start` stores `state`, `nonce`, and a PKCE verifier in a 10-minute `HttpOnly` cookie and redirects to the IdP with an S256 challenge.
4. `/ui/callback` checks `state` in constant time, clears the cookie, exchanges the code with verifier and client secret, validates the ID token and nonce, resolves teams, and stores a `browser` session in `sessions`.
5. `POST /ui/logout` ends the server session (without signing out of the IdP).

### What every page sends

Session cookies use `HttpOnly`, `SameSite=Lax`, and on `https://` URLs `Secure` plus the `__Host-` prefix. Forms are protected by Go's `http.CrossOriginProtection` (`Sec-Fetch-Site` / `Origin`). Every `/ui/` response enforces a strict CSP with no inline scripts/styles or third-party origins:

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

Pages work with JavaScript disabled; `writ.js` adds local-timezone tooltips, relative timestamps, menu toggles, and copy buttons. Styles build on vendored [Oat](https://oat.ink) 1.0.0 with system fonts (16,409 bytes gzipped total across all static assets) and meet WCAG 2.2 contrast ratios in light and dark themes.

## Troubleshooting

- **Redirect URI mismatch in GitLab**: Register both `https://writ.example.com/ui/callback` and `http://127.0.0.1/callback` on separate lines.
- **`writ login` fails over SSH**: Forward the loopback port or use `--device` if your IdP supports device auth.
- **`429` starting sign-in behind a reverse proxy**: Add the proxy's CIDR to `trusted_proxies` so users don't share the 10-pending-login per-address limit.
- **`sessions.json` refused**: File must be a regular file owned by you (`0600` in `0700`). Delete it and run `writ login` again.
- **Offboarded user still has access**: Revoke active sessions immediately via `GET /v1/sessions?person=PERSON_ID` and `DELETE /v1/sessions/{id}`.

## Next steps

- [Set up GitLab CI](set-up-gitlab-ci.md): OIDC auth for CI jobs.
- [API: Token checks](api.md#token-checks): bearer and OIDC validation rules.
- [Plan and apply](apply.md): plan, approve, and apply workflows.
