# Sign in

Let people sign in to a team's `writ serve` from the terminal with `writ login`, or in the browser at `/ui/`. `writ serve` is the OAuth client for both: a laptop holds only a Writ session token, never a client secret, refresh token, access token, or ID token, and a browser holds only its session cookie. CI jobs use OIDC tokens instead ([Set up GitHub Actions](set-up-github-actions.md), [Set up GitLab CI](set-up-gitlab-ci.md)), and `writ serve --dev` needs no sign-in. Protocol detail is in [How sign-in works](how-sign-in-works.md).

## Before you begin

- A `kind = "people"` issuer in the server config ([issuer](config.md#issuer)).
- A confidential OAuth application at your identity provider with scopes `openid profile email`.

## Set up writ login

1. Register the redirect URIs on the OAuth application:
   - `https://URL/ui/callback` for the control UI;
   - `http://127.0.0.1/callback` for browser `writ login`. The IdP must allow any loopback port, per RFC 8252 §7.3. Doorkeeper 5.1+ does, but live GitLab has not been tested end to end.
2. For the device flow, the issuer must publish `device_authorization_endpoint` in `/.well-known/openid-configuration`, and the OAuth app must enable device auth.
3. Restart `writ serve`. With several `people` issuers, users pick one with `--issuer NAME`.

## People: writ login

`writ login URL` uses the device flow (RFC 8628) when the issuer publishes `device_authorization_endpoint`, and otherwise a browser PKCE flow on a loopback port (RFC 8252, RFC 7636), as on GitLab.com. `--device` or `--browser` forces a flow. Both issue the same `cli` session, saved in `~/.config/writ/sessions.json`.

### Sign in with the device flow

```
$ writ login http://127.0.0.1:44701
To sign in, open https://127.0.0.1:40329/oauth/device and enter the code HMEZ-SNDH.
The code expires in 5 minutes.
Signed in to http://127.0.0.1:44701 as bob (gitlab/user/2002).
Session ends 2026-11-11T22:36:29Z.
```

Excerpt of a real run against a local test issuer, 2026-11-11.

### Sign in through a browser

GitLab.com's discovery document omits `device_authorization_endpoint` (checked 2026-11-16), so `writ login` opens a browser on the same machine and receives the callback on an ephemeral loopback port:

```
$ writ login http://127.0.0.1:35309
To sign in, open https://127.0.0.1:36093/oauth/authorize?client_id=writ-e2e&... in a browser on this computer.
The sign-in expires in 10 minutes.
Signed in to http://127.0.0.1:35309 as bob (gitlab/user/2002).
```

Excerpt of a real run against a local test issuer, 2026-11-16. Over SSH, forward the loopback port, or pass `--device` if your IdP supports device auth.

### Check and end a session

```
$ writ whoami --remote
http://127.0.0.1:44701 (from your session), session of bob (gitlab/user/2002, issuer sso), ends 2026-11-11T22:36:29Z
$ writ logout
Signed out of http://127.0.0.1:44701: session 3VGMLAJ2CWONYGAD3SG56GEP4S of bob ended.
```

After `writ logout`, the old token is refused with `auth`. Which server and token a command picks is in [How a CLI finds its control plane](how-sign-in-works.md#how-a-cli-finds-its-control-plane).

> **Warning:** a session lasts `session_ttl` (default `12h`) with no refresh, and its teams are fixed for its life: a person removed from an IdP group keeps the team until the session ends. To cut access at once, revoke the session (below). If `writ logout` cannot reach the server, it deletes the local entry and fails, and the session stays live on the server until it ends ([Sessions](how-sign-in-works.md#sessions)).

## Sign in to the control UI

Open `https://URL/ui/` and sign in through your identity provider. Pages enforce the same grants as the API. Each page is listed in [The control UI](how-sign-in-works.md#the-control-ui).

## Revoke a session

Any principal with `admin` on any grant can list sessions with `GET /v1/sessions?person=PERSON_ID` and revoke one at once with `DELETE /v1/sessions/{id}`.

## Troubleshooting

| Problem | Fix |
| --- | --- |
| Redirect URI mismatch in GitLab | Register `https://writ.example.com/ui/callback` and `http://127.0.0.1/callback` on separate lines. |
| `writ login` fails over SSH | Forward the loopback port, or use `--device` if your IdP supports device auth. |
| `writ login --browser` says the server has no browser sign-in | That `writ serve` is too old for it. Use `--device`. |
| `429` when starting sign-in behind a reverse proxy | Add the proxy's CIDR to `trusted_proxies`, so users do not share one address's limit of 10 pending logins ([Sign-ins that wait](how-sign-in-works.md#sign-ins-that-wait)). |
| `sessions.json` refused | It must be a regular file you own, `0600` in a `0700` directory. Delete it and run `writ login` again. |
| `writ: no control plane` or `no token for URL` | Sign in with `writ login URL`; in GitHub Actions give the job `permissions: id-token: write`; in GitLab CI set `id_tokens: { WRIT_ID_TOKEN: { aud: URL } }` on the job. |
| An offboarded user still has access | Revoke their sessions: `GET /v1/sessions?person=PERSON_ID`, then `DELETE /v1/sessions/{id}`. |

## Next steps

- [How sign-in works](how-sign-in-works.md): flows, pending limits, sessions, and control UI headers.
- [Set up GitHub Actions](set-up-github-actions.md) and [Set up GitLab CI](set-up-gitlab-ci.md): OIDC auth for CI jobs.
- [API: Token checks](api.md#token-checks): bearer and OIDC validation rules.
