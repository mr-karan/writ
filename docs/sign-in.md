# Sign in

To use a team's control plane, you sign in from a terminal with `writ login`, or in a browser through the control UI. Both use your team's identity provider, and `writ serve` is the OAuth client for both. So your laptop holds a session and no refresh token, and your browser never holds the client secret, an access token, or an ID token.

`writ login` uses the device flow when your identity provider offers it. When it doesn't, as on GitLab.com, `writ login` signs you in through a browser on the same computer, as [Sign in through a browser](#sign-in-through-a-browser) explains.

CI jobs don't sign in. Instead they send the OIDC token their platform issues, as [Set up GitLab CI](set-up-gitlab-ci.md) shows. Under `writ serve --dev` you don't sign in at all.

## How a CLI finds its control plane

When you run a remote command, `writ login`, or `writ logout`, it uses the first of these that's set:

1. `--server URL`, or `--server dev` for the dev control plane. Passing `--server` implies `--remote`, and a command with no remote form refuses it.
2. `WRIT_SERVER`.
3. Whichever one control plane you have a live `writ login` session for. If you have sessions for two, the command stops and names both.
4. Your dev control plane, when `client.json` is in the default data directory and its server answers `GET /v1/server` with `dev: true`.

Your URL must be `https://`, or `http://` on a loopback address, with no path, user, query, or fragment, because the client sends a token to it. For the server chosen, the client sends the first of these tokens that's present:

1. In CI, `WRIT_ID_TOKEN`, the job's OIDC token.
2. Your session from `writ login` for that URL, in `~/.config/writ/sessions.json`.
3. Your dev token from `client.json`, when the server is the dev control plane.

With no server, the command stops with exit code 2. With a server and no token, it tells you how to get one:

```
$ writ whoami
writ: no control plane: start one with writ serve --dev, or sign in to your team's with writ login URL
run writ help for usage
$ writ runs --remote --server http://127.0.0.1:41093
writ: no token for http://127.0.0.1:41093: sign in with writ login http://127.0.0.1:41093, or in GitLab CI set id_tokens: { WRIT_ID_TOKEN: { aud: http://127.0.0.1:41093 } } on the job
run writ help for usage
```

Output of a real run, 2026-11-16.

To see which server and token a command picked, run `writ whoami`: its first line says where each came from. If you started a dev control plane with `--data`, it keeps its `client.json` in that directory, where a client doesn't look. Point `XDG_DATA_HOME` at the directory's grandparent instead.

## People: writ login

Sign in from a terminal with `writ login`. Behind it, `writ serve` is the OAuth client of an issuer of kind `people`, through `golang.org/x/oauth2`, in one of two flows:

- The device authorization grant of RFC 8628, when the issuer's discovery document lists a `device_authorization_endpoint`.
- The authorization code flow with PKCE and a loopback redirect, [through a browser](#sign-in-through-a-browser), when it doesn't.

`--device` and `--browser` force a flow. Both end in the same session. Your laptop never holds the client secret, an access token, or an ID token. It holds a session and no refresh token.

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

Output of an end-to-end test, 2026-11-11, with the real `writ` binary as client and server and a test issuer in place of GitLab. That test has since been removed. Still, every line it prints is what the code prints, and `TestLoginWhoamiLogout` in `internal/cli` covers the same flow. After `writ logout`, the control plane refuses the old session token with an `auth` error, so a copy of it is no use.

### What happens when you sign in

1. First, `writ login` asks `GET /v1/server` whether sign-in is set up, then calls `POST /v1/auth/device`. In turn, `writ serve` calls the identity provider's `device_authorization_endpoint` with its client id and secret, and returns `writ.device/v0`: the user code, the verification URL, how often to poll, when the code expires, and a handle of 32 random bytes. Your device code stays in the `writ serve` process.
2. Your CLI prints the control plane's URL, the verification URL, and the code, and polls `POST /v1/auth/device/token` with the handle. For each sign-in, one goroutine in `writ serve` polls the identity provider's token endpoint through `golang.org/x/oauth2`. That goroutine polls at the interval the provider gave, and 5 seconds slower after each `slow_down`, whatever the client sends. Your client's poll reads what that goroutine found.
3. When you sign in, `writ serve` checks the ID token as the control UI's sign-in does, without the nonce, which the device flow doesn't send: issuer, signature, times, and audience. Then it reads the groups from userinfo when `groups` says so, and maps the claims to teams. Next it stores a session with carrier `cli`: the SHA-256 of a token of 32 random bytes, the person, the teams, and the end time. That one answer, `writ.session/v0` with status `signed-in`, carries the token.
4. Your CLI keeps the session in `~/.config/writ/sessions.json` (`$XDG_CONFIG_HOME/writ` when set), mode 0600 in a directory of mode 0700, under the control plane's URL. For safety, it refuses to read the file when it's a symbolic link, isn't a regular file, belongs to another user, or others can read it, and it tells you to delete it and sign in again. When it writes the file, it sets the directory's mode to 0700 if it was looser. If you sign in again to the same control plane, the new session ends the one it replaces.

Your handle works once: the sign-in ends when you sign in or refuse, when the code runs out, or when the ID token fails a check. If `writ serve` restarts, it drops the sign-ins that wait, so you run `writ login` again. [Sign-ins that wait](#sign-ins-that-wait) has the limits on them.

### Sign in through a browser

GitLab.com's [discovery document](https://gitlab.com/.well-known/openid-configuration) lists the `device_code` grant type but no `device_authorization_endpoint`, read on 2026-11-16. So on GitLab.com, `writ login` prints a URL, you open it in a browser on the same computer, and the identity provider sends your browser back to a port `writ login` listens on. It follows RFC 8252 for native apps and RFC 7636 for PKCE.

```
$ writ login http://127.0.0.1:35309
Signing in to http://127.0.0.1:35309 through issuer sso.
To sign in, open https://127.0.0.1:36093/oauth/authorize?client_id=writ-e2e&code_challenge=QqueYeHu6tMmOgOABEnQeLVDZ4E2tW41WqKkByO5AdM&code_challenge_method=S256&nonce=BV_F4WYQLxLGVMYFi9phIfQaGZKgk6l7-QCRw0TZzEU&redirect_uri=http%3A%2F%2F127.0.0.1%3A36443%2Fcallback&response_type=code&scope=openid+profile+email&state=BGX7AJ5XD2T6KK7AUC6OMKYHR2 in a browser on this computer.
The sign-in expires in 10 minutes.
Signed in to http://127.0.0.1:35309 as bob (gitlab/user/2002).
Teams: acme
Session ends 2026-11-16T20:39:54Z.
```

Output of `TestTofuApproveApply` in `e2e`, 2026-11-16, with the real `writ` binary as client and server, a test issuer that offers no device flow in place of GitLab, and a scripted browser that follows the URL.

Here's what happens:

1. `writ login` listens on `127.0.0.1`, on a port the kernel picks, and creates a random `state` and a PKCE code verifier through `golang.org/x/oauth2`. It calls `POST /v1/auth/browser` with the redirect URI `http://127.0.0.1:PORT/callback`, the state, and the verifier's S256 challenge. The verifier stays in the CLI until the code comes back.
2. `writ serve` checks the request. It refuses a redirect URI that isn't exactly `http://127.0.0.1:PORT/callback`, such as one on `localhost` or `[::1]`, on another path, or with a query. It refuses a challenge that isn't 43 characters of S256. Then it creates a nonce, keeps the sign-in in memory for 10 minutes under a handle of 32 random bytes, and returns `writ.browser/v0` with the identity provider's URL. That URL carries the client id, the redirect URI, the state, the nonce, and the challenge.
3. The CLI prints the URL, which is always enough. When you read its prompts at a terminal, it also opens the URL: with `xdg-open` on Linux when `DISPLAY` or `WAYLAND_DISPLAY` is set, `open` on macOS, or `rundll32` on Windows. It refuses a URL that isn't `https://`.
4. The identity provider signs you in and sends your browser to the loopback port. The listener serves that one request and closes, whatever the request contains. It refuses a request from an address that isn't loopback, a `Host` other than `127.0.0.1:PORT`, as a DNS rebinding page would send, another path, another `state`, or the identity provider's `error`. Your browser gets a plain text page that says what happened. When no request comes within the 10 minutes `writ serve` gave the sign-in, the CLI stops listening and fails, so a later callback finds no listener.
5. The CLI calls `POST /v1/auth/browser/token` with the handle, the code, the state, and the verifier. The handle ends at this request, whatever its result. `writ serve` compares the state in constant time and checks that the verifier's S256 challenge is the one the sign-in started with. Then it trades the code with the verifier and its client secret. It checks the ID token as the control UI's sign-in does: issuer, signature, times, audience (the `client_id`), and the nonce it created. The identity provider refuses a code that was already used.
6. `writ serve` issues the session the device flow issues: carrier `cli`, the same `session_ttl`, the same person, and the same audit row, kind `sign-in` with `via` set to `cli session`. The CLI keeps it as in step 4 of the device flow.

Your browser must run on the computer where `writ login` runs, because the identity provider sends it to that computer's loopback address. Over SSH, forward the port, or use `writ login --device` with an identity provider that offers the device flow.

A `writ serve` from before browser sign-in sends no `device_issuers` in `GET /v1/server`, so `writ login` uses the device flow with it. If you pass `--browser` to such a server, `writ login` says the server is too old for browser sign-in, and suggests `--device`.

### Sign-ins that wait

`writ serve` keeps each sign-in that waits for you in memory, until you finish it or it runs out. Neither flow's two routes take a token, so the limits below protect the places that sign-ins take:

- At most 1,000 device sign-ins and 1,000 browser sign-ins wait at once. A device sign-in takes its place before `writ serve` asks the identity provider for a code, and gives it back when the provider refuses. So sign-ins that start at the same moment can't pass the limit.
- At most 10 sign-ins from one client address wait at once, in both flows together. The client address is the one [the address rate limit](api.md#rate-limits) counts: an IPv6 address counts by its /64. You need one sign-in at a time, and one you use ends within a minute, so 10 leaves room for an office behind one NAT address. Past it, `writ serve` returns 429 and says how many wait from that address.
- A start whose audit row can't be written returns 503 and gives its place back.
- Both flows' routes count against `limits.requests_per_second` for each client address.

**Limits.** The per-address limit stops one address from taking every place, but many addresses together can still fill the 1,000. Then every new sign-in gets 429 until theirs run out, after 10 minutes for a browser sign-in or the device code's life. Behind a proxy that isn't in `trusted_proxies`, every client counts as the proxy's address, so all of them share the 10.

### Sessions

- Your session lasts `session_ttl`, 12 hours by default. There's no refresh: you run `writ login` again.
- Teams are fixed for the session's life. If you remove a person from a group at the identity provider, they keep the team until the session ends.
- A session from `writ login` is a bearer token on every `/v1` route that takes a token, and its audit rows say `via` `cli session`. Writ refuses a browser session from the control UI as a bearer token, and a `writ login` session opens no control UI page.
- `writ logout` sends `DELETE /v1/auth/session` and deletes the session from the file. When `writ serve` can't be reached, it still deletes it, and fails, because the session stays live there until it ends.
- To offboard a person, a principal with `admin` lists the live sessions with `GET /v1/sessions`, or one person's with `?person=`, and ends one at once with `DELETE /v1/sessions/{id}`. Any grant that gives `admin` counts, whatever its stacks and environments.
- The token appears in the answer that issues it and in the session file, and nowhere else: not in the log, an audit row, a page, or another answer. Both the server and e2e tests scan each one for it.
- The CLI follows no redirect from a control plane. Go's HTTP client keeps the `Authorization` header on a redirect to the same host name, whatever the port or the scheme, so a redirect could carry the token over plain HTTP or to another server on that host. Set the address `writ serve` answers on with `--server` or `WRIT_SERVER`.

### Set up writ login

`writ login` works with every issuer of kind `people`. With several people issuers, `writ login --issuer NAME` picks one. Under `writ serve --dev` you don't sign in, and `writ login` says so. What you set up depends on the flow your identity provider offers.

**The device flow.** Your issuer must publish a `device_authorization_endpoint` in its discovery document, and its application must allow the device flow. Writ reads that endpoint only from the discovery document. For a GitLab self-managed instance, look for it in the instance's own `/.well-known/openid-configuration`.

**Through a browser.** Register `http://127.0.0.1/callback` as a redirect URI of the application whose id is `client_id`, beside the control UI's `URL/ui/callback`. GitLab's form takes one redirect URI per line. The application stays confidential, because `writ serve` keeps the client secret.

The CLI's port changes on each sign-in, so the identity provider must accept a loopback redirect URI on any port, as RFC 8252, section 7.3, says it should. For GitLab, that's inferred, not tested:

- GitLab's documentation doesn't say whether it does: [`docs.gitlab.com/integration/oauth_provider/`](https://docs.gitlab.com/integration/oauth_provider/) and [`docs.gitlab.com/api/oauth2/`](https://docs.gitlab.com/api/oauth2/), read on 2026-11-16.
- GitLab's `Gemfile.lock` pins Doorkeeper 5.9.9. Doorkeeper's changelog says release 5.1.0 matched loopback redirect URIs on any port, per RFC 8252, section 7.3. Both were read on 2026-11-16.
- No run of `writ login` has used a real GitLab. If GitLab refuses the redirect URI, it doesn't send your browser to it (RFC 6749, section 4.1.2.1), and `writ login` fails when its 10 minutes end.

**Limits.** Whether one GitLab application serves the control UI's code flow, the device flow, and the loopback redirect together isn't tested, because no run used a real GitLab.

## The control UI

In a browser, you use the control UI: the set of web pages that `writ serve` renders under `/ui/` for people. A request for `GET /` redirects there. Each page renders one or two of the API's documents, and gets them by running the same `/v1` handler with your session. So a page and the API decide and answer the same way, and each read leaves the same audit row, with `via` set to `browser session`. At the bottom of every page, the footer shows the API requests and the CLI command that print the same document, and links the main document as JSON with `?as=json`.

Every page sits beside a sidebar: the sections, the count of plans that wait for approval, plans that apply, and repositories, then each environment with its stacks, each with a dot and its words, such as `1 pending · 1 applying` or `drifted`. A filter box narrows the stacks by name with `?find=`, and the sidebar's last line says when a repository was last polled, or how many polls failed. It reads `writ.overview/v0` once, which the overview and the environment tabs of a list reuse. That read leaves an audit row like any other, and the footer leaves it out unless it is the page's own. The top bar shows the trail to the page, such as environment, stack, and plan. On a narrow screen the sidebar folds under a Menu button.

The request for the page leaves a row of its own too, with its `/ui/` route and you as the person. So a page writes one row, plus one for each read. If a page's main read can't write its row, the page shows the 503. If the page's own row can't be written, you get a 503 page that sets no cookie and redirects nowhere. A sign-out that can't end its session shows that page too: you keep your cookie, and the session stays live. The reads of `GET /v1/server` and `GET /healthz` on `/ui/settings` and `/ui/start` leave no row. [Every answer waits for its audit row](api.md#every-answer-waits-for-its-audit-row) has the rule.

| Page | Documents | What it shows |
| --- | --- | --- |
| `/ui/` | `writ.overview/v0` | Four counts: plans that wait for approval, plans that apply, runs that failed in the last 24 hours, and stacks whose last drift check found drift. Then the alerts: drift found, a drift check that failed, a state written outside `writ serve`, a failed run, a pending or approved plan that expires within 2 hours or a quarter of the plan lifetime, and a repository poll that failed. Then the plans that apply, wait, and plan, each with its `stack@env`, commit, author, and age, and the first 200 OpenTofu `stack@env`s with state, each with its backend, resources, and drift. A count read from a list `writ serve` cut short carries a `+`, and the page never says all is clear when such a list could hold an alert |
| `/ui/stack?stack=&env=` | `writ.state-index/v0`, `writ.drift/v0`, `writ.remote-runs/v0`, `writ.remote-plans/v0` | Resources, runs, and plans. Never a state value. An OpenTofu stack's page shows its newest [drift check](serve.md#drift-checks), each changed resource with its kind, action, and changed attributes, the S3 mark of a state written outside Writ, and, in [the Resources view](serve.md#the-resources-view), the rows of its state index. |
| `/ui/runs?env=&stack=`, `/ui/runs/{run}?stack=&env=` | `writ.remote-runs/v0`, `writ.remote-run/v0` | The runs `writ serve` started in an environment, with their `stack@env`. A run of an OpenTofu plan shows its status, its plan, its approvals, its timing, what the apply did to each resource from its masked events, and the masked diagnostics of a failed apply. |
| `/ui/plans`, `/ui/plans/{id}` | `writ.remote-plans/v0`, `writ.remote-plan/v0` | Plan review of an OpenTofu plan, from its masked document only: each change by module, then address, with its actions, `action_reason`, `replace_paths`, import marker, risk classes, and its attributes before and after, a value under a sensitive mark as `(sensitive)`, then the drift and the outputs that change. A plan that couldn't be created shows its diagnostics. A plan that a newer one from the same merge request superseded says so. A plan an older Writ created of a stack of the Writ language shows only a note that says so, and offers no approval. |
| `/ui/approvals` | `writ.remote-plans/v0` | Pending plans by others and by you. A plan's page approves it. |
| `/ui/access`, `/ui/whoami` | `writ.config/v0`, `writ.whoami/v0` | Every team's rule and every grant, and what you can do |
| `/ui/settings` | `writ.server/v0`, `writ.health/v0`, `writ.config/v0` | The server, its health, its config with its forges, and the theme of this browser |
| `/ui/start` | `writ.server/v0`, `writ.repos/v0`, `writ.repo/v0`, `writ.remote-plans/v0` | Getting started: the control plane, the repositories, the stacks found, the first plan, and the next step |
| `/ui/repos`, `/ui/repos/{name}` | `writ.repos/v0`, `writ.repo/v0` | Each repository with its last commit read and last poll, and one repository's stacks found |
| `/ui/repos/new` | `writ.repo/v0` | Add repository: a form that `POST /v1/repos` answers. It takes no token. |

To see the control UI and its redirect URI, look at the `control UI:` line of `writ serve --check`, as the output in [Check the isolation](serve.md#check-the-isolation) shows.

**Limits.** You won't find the audit log in the control UI, because the API doesn't serve one yet. A run's page shows what the apply did and how it ended once the run ends, and reloads every 5 seconds while it applies: no page streams OpenTofu's events as they happen. No run has signed in through a real GitLab: the tests use a test issuer.

### Signing in

You sign in with the authorization code flow with PKCE against an issuer of kind `people`. Again `writ serve` is the OAuth client, so your browser never holds the client secret, an access token, or an ID token.

1. Register `URL/ui/callback` as a redirect URI of the application whose id is `client_id`, with the scopes `openid profile email`. GitLab lists the application under Applications, in the user, group, or instance settings.
2. When you open a page without a session, it answers `303` to `/ui/login?next=PATH`, and you pick the issuer.
3. Then `/ui/login/start` creates a random `state`, `nonce`, and code verifier, keeps them in an `HttpOnly` cookie that lives 10 minutes, and redirects to the issuer with the S256 challenge. Writ sends the challenge even when the discovery document doesn't list S256, and `writ serve` logs a warning at start.
4. On return, `/ui/callback` compares `state` in constant time and deletes the cookie, so a callback works once. Then `writ serve` trades the code with the verifier and the client secret, and checks the ID token's issuer, signature, times, audience (the `client_id`), and nonce. Next it reads the groups from userinfo when `groups` says so, and maps the claims to teams.
5. Finally it stores a session in the `sessions` table with carrier `browser`: the SHA-256 of a random token, the person, the teams, and the end time. Your cookie has the token. Teams are fixed for the session's life.

You are `IDENTITY/user/SUB`, such as `gitlab/user/2002`. If you start a GitLab CI job, its token carries the same id, as `user_id`, when its issuer has the same `identity`. Once cleaned, `next` must be a path under `/ui/`, or you land on `/ui/`. To sign out, `POST /ui/logout` ends the session on the server. That doesn't sign you out of the identity provider. When a session ends, a `GET` goes to sign-in and back to the same page, and a form post is dropped.

Under `/v1`, Writ takes no cookie and sets none. If you send a browser session as a bearer token, Writ refuses it with `auth`: only a session from [`writ login`](#people-writ-login) is a bearer token.

### What every page sends

Your cookies are `HttpOnly`, `SameSite=Lax`, and, on an `https://` URL, `Secure` with the `__Host-` prefix. On a loopback `http://` URL, for a test or a local proxy, they have neither, because both need HTTPS. Each response under `/ui/` carries a Content Security Policy that allows no inline script or style and nothing from another origin. On top of that, `http.CrossOriginProtection` refuses a form post from another site, by its `Sec-Fetch-Site` or `Origin` header. Here are the headers of `/ui/whoami` for a signed-in client of a dev control plane that the UI showcase served, `200 OK`:

```
Cache-Control: no-store
Content-Security-Policy: default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'
Content-Type: text/html; charset=utf-8
Referrer-Policy: no-referrer
X-Content-Type-Options: nosniff
Date: Mon, 16 Nov 2026 06:22:31 GMT
Transfer-Encoding: chunked
```

As the showcase's client got them, 2026-11-16.

Pages read without JavaScript. Its one script, `writ.js`, adds each time in the reader's zone as the time's title, keeps the top bar's "Loaded" time relative, opens the sidebar's menu on a wide screen, and adds a copy button beside a command. Your theme is a cookie that the settings form sets, and the server renders it, so no script runs before the page paints. For styles, Writ uses [Oat](https://oat.ink) 1.0.0, with Writ's own stylesheet over it, vendored with its SHA-256 in `internal/ui/static/VENDOR`. Text is set in the system's own faces, so the UI loads no font and its policy has no `font-src`. The stylesheets, script, and licenses are embedded in the binary: 16,409 bytes gzipped, measured by `TestAssetSize` on 2026-11-16. `internal/ui/DESIGN.md` says why the pages look as they do. In both themes, the pages meet the contrast ratios of WCAG 2.2, which `TestContrast` computes from the two stylesheets.

## Next steps

- [Set up GitLab CI](set-up-gitlab-ci.md): sign-in for CI jobs.
- [API](api.md#token-checks): how `writ serve` checks every token.
- [Approve and apply a plan](apply.md).
