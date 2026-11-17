# Approve and apply a plan

Once `writ serve` has a commit's plan, people approve it, and then you apply it by its id or after the merge. Below are the commands, what to do when an apply loses its answer, and how to override the approval rules.

For how approval rules work and who can approve, read [Approvals](approvals.md) first.

## Approve a plan

You approve from your laptop, with your [`writ login`](sign-in.md#people-writ-login) session. If a grant gives your team `apply`, you apply by id the same way. In the control UI, the plan's page has an Approve button for a signed-in person who didn't create the plan, and it sends the same request.

When you run `writ approve --remote ID`, it prints one line for each plan it approved, as `Approved plan ID for ENV (STACK): N of M approvals (WHO), STATUS.` Writ records your stable id as the actor, such as `gitlab/user/2002`, and its auth, `person`. To take an approval back, run `writ approve --remote --withdraw ID`. You can do that until the plan starts applying.

In GitLab CI, your merge request job plans each stack, and the plan's text is the masked plan. Here's the text of the plan from alice's merge request job:

```
OpenTofu 1.13.9 plan of app@prod

  + random_pet.name (create)
  + terraform_data.note (create)
  + terraform_data.secret (create)
  + module.cert.tls_private_key.key (create)

Plan: 4 to add, 0 to change, 0 to destroy.
Outputs that change: pet.
```

Output of `TestTofuApproveApply`, 2026-11-16, against a team's `writ serve` with a test issuer and a stand-in of GitLab.

Then bob, a maintainer, runs the approve job of the same pipeline, `writ approve --remote --pipeline`. That job approves every live plan of the pipeline, all or none. If alice runs the same job, Writ refuses it with exit code 1, and the plan stays pending.

## Apply a plan

To apply, you need `apply` on the plan's `stack@env` and every risk class of its changes, from one grant or several. Your run applies the stored saved plan, with the environment's `apply_role`, and with its own provider uid when your config sets `provider_uids`. To read it, `writ serve` opens the saved plan with the environment's key, and it opens only as the plan it was made for.

Your client follows the run until it ends. Press Ctrl-C once and you stop following, but the run goes on. To see it later, run `writ show --remote RUN`.

### Apply by id

To apply one plan, run `writ apply --remote ID`:

```
$ writ apply --remote 931d8c004160c9e54cd554d70d1df149
Run VL7RNRTHG3SYT7GRXC5O23VWV6 of plan 931d8c004160c9e54cd554d70d1df149 for dev (app), by alice (dev).
OpenTofu apply of app@dev
  random_pet.name: create complete
Apply: 1 added, 0 changed, 0 destroyed.
State version 1 of app@dev.
Run VL7RNRTHG3SYT7GRXC5O23VWV6 of plan 931d8c004160c9e54cd554d70d1df149 for dev (app), by alice (dev): applied at 2026-11-16T03:29:49Z.
```

Output under `writ serve --dev`, 2026-11-16.

From a CI job, you can apply by id only a plan of the job's own pipeline, with the same issuer, project, and pipeline, or a plan its pipeline got as the same plan as an earlier pipeline's. That's for a team that applies before the merge, such as on staging.

### Apply after a merge

Run `writ apply --remote --merged --env ENV --stack NAME`. That applies the approved plan of the stack whose sources equal the merged commit's.

From a GitLab CI job, the commit is the token's `sha`, and the job must run on the repository's default branch. Anyone else sets `--repo` and `--commit`. Either way, `writ serve` checks at the Git host that the commit is on the default branch.

Here's how it finds the plan. First `writ serve` reads the stack's files at the merged commit, as a plan of that commit would, and looks up the live commit plans of the repository's `stack@env` with that digest in the control database. Then the newest approved one applies, however many other live plans the `stack@env` has.

Because `writ serve` weighs every live plan of the digest, newer plans whose approvals don't count never hide an older plan whose approvals do. Since it compares sources, not commit ids, it works for merge commits, squash merges, and fast-forward merges.

Here, under `writ serve --dev`, a person sets the commit:

```
$ writ apply --remote --merged --stack app --repo demo --commit d9a4b37495d9d1c581d07a7895d9ef3a3401f4c2
Run T5QKSTXGRYE66SZBECBPGUV7QV of plan f38f9206b30b2f7ffbcaca647a9b8190 for dev (app), by alice (dev).
OpenTofu apply of app@dev
  random_pet.name: create complete
Apply: 1 added, 0 changed, 0 destroyed.
State version 1 of app@dev.
Run T5QKSTXGRYE66SZBECBPGUV7QV of plan f38f9206b30b2f7ffbcaca647a9b8190 for dev (app), by alice (dev): applied at 2026-11-16T03:34:58Z.
```

Output of a real run, 2026-11-16.

If no approved plan matches, the job fails and nothing changes:

- If a plan with the same sources lacks approvals, Writ refuses it as `denied`, with its id.
- If the sources equal the last applied plan's, Writ refuses it as `saved-plan`, `nothing to apply`.
- Any other sources, including the code of a stack it reads, are refused as `saved-plan`.

Here's the same commit again, and then a later commit that no plan was created for, on the same control plane:

```
$ writ apply --remote --merged --stack app --repo demo --commit d9a4b37495d9d1c581d07a7895d9ef3a3401f4c2
error: saved-plan: nothing to apply: plan f38f9206b30b2f7ffbcaca647a9b8190, applied last to app@dev, has the sources of demo at d9a4b374
$ writ apply --remote --merged --stack app --repo demo --commit de72518e5a53e44a1fd8ed6389cb663a844dd929
error: saved-plan: no approved plan of app@dev matches the sources of demo at de72518e: plan the merge request again after a rebase, and approve the new plan
```

If the author removes the approve job from the branch, the merge request can merge, and the job on `main` finds no approved plan. So `main` has code that isn't applied until someone approves the plan and runs the job again. The control plane, not the pipeline, is the gate.

### Plan and apply in one command

On the dev control plane, you can do both at once. Run `writ apply --remote --yes`, and it plans the commit as `writ plan --remote` does, and applies the plan when no rule needs an approval. If the plan needs approvals, the command prints it and exits 1.

With `--json`, stdout has one document: the plan when it needs approvals and nothing runs, else the run. Your approved plan runs even with no resource change, because it may change outputs.

If you interrupt while the command follows the run, it stops following, the run goes on, and the command exits 1 with the failure document, which identifies the run.

## When an answer is lost

Your request to apply can start a run and lose its answer. Your connection can break, or the database can commit the run after the client stops its wait of 30 seconds. So your client sends a key, and the key tells it whether a run started.

In most cases you don't have to do anything. On its own, `writ apply --remote` asks `writ serve` and follows the run if one started. If it prints a key and a command, run that command later to find out.

That command, `writ show --remote --request KEY`, asks the outcome route. You see the run, or a note that no run started, and it also closes a key with no run.

Read on to learn how the key works, so you can trust what the client tells you.

### How the key works

For each command, `writ apply --remote` creates one key, and sends it in the `Idempotency-Key` header of each request to apply: by id, after a merge, and with `--yes`. Your key is the Unix time in seconds, a dash, and 26 random characters from `crypto/rand`, such as `1794405508-IDTB4TY5M2Z4M4RFR773KN4BKQ`. If your request has no key, `writ serve` creates one.

Every key has one row, and the row is final. Writ writes the key in the transaction that starts the run, as its first write. That row says one of three things:

- `started`: a request with the key started a run. If the same principal repeats the same request, it gets 202 with the same run. That starts no second run and spends no second CI token. The key with another request is refused as `usage`, and from another principal as `denied`.
- `none`: the outcome route, or `writ serve` after a failed commit, closed the key with no run. A request with the key is refused as `state` with `key_closed: true` and starts no run.
- No row: no request with the key started a run. If Writ refuses a request before its commit, it writes no row, so the command can send the key again.

### Ask whether a run started

To ask, call `POST /v1/apply-requests/{key}/outcome`. It returns `started` with the run, or `none`. For a key with no row, it first writes a `none` row, and answers after that write commits.

On Postgres, the write waits for a transaction that holds the same key, so the route can't answer `none` while the commit of a run waits. On SQLite, the write lock does the same. So `none` is final: no later request with the key starts a run. Asking closes a key with no run, so to ask is to cancel.

### What writ apply does for you

After a transport error, a timeout, or a 503 whose `outcome` isn't `none`, `writ apply --remote` asks the outcome route, at most 5 times:

- With `started`, it prints the run and follows it.
- With `none`, it prints that no run started and exits 1. Run the command again: it creates a new key.
- When the route doesn't answer, it prints the key and the command that asks later, and exits 1.

After a 429, or a 503 with `outcome` `none` and no `key_closed`, it sends the same request with the same key again. After a 503 with `outcome` `none` and `key_closed: true`, or a 409 with `key_closed: true`, it sends the same request with a new key, because no request with the closed key starts a run. These sends share the 5 tries. A 503 with `outcome` `unknown` never gets a new key.

You can see each case below, from `TestRemoteApplyOutcome`, run against a stand-in of `writ serve` on 2026-11-16. In them, the run's text, `Applied 1 change in 5ms.`, and its end time are the stand-in's. First, a stand-in that drops the connection after the run's transaction commits:

```
$ writ apply --remote d3f700d860a97483a7ad509c31fe9e28
The apply request got no answer: POST http://127.0.0.1:41971/v1/runs: EOF
Asking writ serve whether it started a run.
Run LLGTGDJCKQ6LZQJWNBMA2ESMHV of plan d3f700d860a97483a7ad509c31fe9e28 for prod (network), by alice.
Applied 1 change in 5ms.
Run LLGTGDJCKQ6LZQJWNBMA2ESMHV of plan d3f700d860a97483a7ad509c31fe9e28 for prod (network), by alice: applied at 2026-11-11T10:00:05Z.
```

Next, when the request started no run, and when the outcome route doesn't answer:

```
$ writ apply --remote d3f700d860a97483a7ad509c31fe9e28
The apply request got no answer: POST http://127.0.0.1:42773/v1/runs: EOF
Asking writ serve whether it started a run.
error: state-io: no run started: writ serve closed the key 1794800081-K7VCN743YGPEVSILJOOIIB4H6V of this command's requests with no run; run the command again
$ writ apply --remote d3f700d860a97483a7ad509c31fe9e28
The apply request got no answer: POST http://127.0.0.1:36599/v1/runs: EOF
Asking writ serve whether it started a run.
error: usage: no route POST /v1/apply-requests/1794800081-PM5SZ64M6ISVYOSYKVCKABCWOC/outcome
error: state-io: writ serve did not say whether the apply request with the key 1794800081-PM5SZ64M6ISVYOSYKVCKABCWOC started a run; ask later with: writ show --remote --request 1794800081-PM5SZ64M6ISVYOSYKVCKABCWOC
```

`TestApplyKeyClosedBySettle` runs a real `writ serve` whose first commit of a run's start fails and rolls back. Then `writ serve` settles the key to `none` and returns 503 with `key_closed: true`, and the client sends the request again with a new key. Against the stand-in, which answers the same way:

```
$ writ apply --remote d3f700d860a97483a7ad509c31fe9e28
writ serve closed the key 1794800081-RVCR4ZVQFGWXKR2LWDN5VV75BT with no run. Sending the request again with a new key.
Run LLGTGDJCKQ6LZQJWNBMA2ESMHV of plan d3f700d860a97483a7ad509c31fe9e28 for prod (network), by alice.
Applied 1 change in 5ms.
Run LLGTGDJCKQ6LZQJWNBMA2ESMHV of plan d3f700d860a97483a7ad509c31fe9e28 for prod (network), by alice: applied at 2026-11-11T10:00:05Z.
```

### What each 503 means

When `POST /v1/runs` returns 503, its `outcome` is `none` if the request started no run, and `unknown` if `writ serve` can't tell. A 503 of the outcome route has `outcome` `unknown`.

Only two answers promise that no request with the key starts a run: `none` from the outcome route, and `key_closed: true` on a refusal of `POST /v1/runs`. If you get a 503 with `outcome` `none` and no `key_closed`, it comes from a failure before the commit. In that case the key has no row, and the same key can start a run.

### When the commit returns an error

In that case, `writ serve` asks the database for the key's outcome, as the outcome route does, with 5 seconds for the attempt:

- With `started`, the run goes on and the answer is 202.
- With `none`, the answer is 503 with `outcome` `none` and `key_closed: true`, and the key is closed.
- When the attempt fails, the answer is 503 with `outcome` `unknown`. Meanwhile `writ serve` keeps the apply slot and asks again every 5 seconds, for at most 2 minutes. `started` then starts the run's work. `none`, the end of the 2 minutes, and the stop of `writ serve` give the slot back.

If a run started and `writ serve` gave up on it, the run stays `applying` with no work behind it until the next sweep marks it `failed`, as [What a run records](approvals.md#what-a-run-records) says. So a database outage holds an apply slot for at most 2 minutes.

### Who can learn a key's outcome

For a `started` key, the outcome route answers the principal that started the run, by issuer and subject, and any principal with `admin`. Writ refuses another principal as `denied`, with an audit row, and that refusal shows that the key started a run. For a key with no run, it answers any principal with a valid token, and closes the key.

So if someone learns a key before its run starts, from a log or the output of a CI job, they can close it, and the apply with that key then starts nothing. With 130 random bits, nobody can guess a key. In GitLab CI, a job's subject identifies its project and branch, not its pipeline, so the jobs of one branch are one principal here.

### How long a key lives

Your key starts a run for 24 hours after the time it contains. Writ refuses an older key as `usage`, before it reads the key's row, and also a key more than 5 minutes ahead of its own clock.

Once written, a `started` row stays as long as its run. Each hour, the prune deletes a `none` row 25 hours after its key's time. By then the key is refused, so a replay of a key whose `none` row is gone starts no run. `TestPruneThenReplay` proves it.

The prune also guards against a clock that moves back. It keeps the greatest time of a key whose `none` row it deleted, in the transaction of the delete, and that time never goes down. Later, the transaction that starts a run refuses a key at or below that time as `state` with `key_closed: true`, whatever the clock of `writ serve` says. So a clock that moves back doesn't reopen a key.

Because a key contains whole seconds, a new key created in the same second as that time is refused too. `TestPruneThenClockBack` and `TestPruneFloor` prove it on SQLite and Postgres.

## Override the approval rules

Sometimes you need to apply before the approvals arrive, such as to rotate a leaked key while the approver is away. If you have `override`, you can apply a plan without the approvals its rules need, with a reason of 10 to 500 characters:

```
writ apply --remote ID --override "bob on leave, rotate leaked key, OPS-112"
```

- Your override skips the approval rules only. You still need `apply` and every risk class of the plan.
- A CI job can't give one. If a plan already has its approvals, Writ refuses an override, as `usage`, so the record shows only real overrides.
- Writ records your reason in the run, the plan's history, the audit row, the log at `WARN`, and the stack's state.

## Limits

- `limits.applies`, 4 by default, bounds the runs that apply at once. Past it, you get 429. Applies share the provider uids with plans and wait for a free one.
- `limits.apply_start_timeout`, 10 minutes by default, bounds that wait. `limits.apply_timeout`, 4 hours by default, stops a running apply from starting more changes, and lets the changes that are running finish. See [Time limits](api.md#time-limits). An apply of an OpenTofu plan stops the same way, through OpenTofu's own interrupt.
- A start whose commit failed holds its apply slot while `writ serve` asks for the key's outcome: every 5 seconds, 5 seconds for each attempt, for at most 2 minutes.
- A key starts a run for 24 hours after the time it contains, and can be at most 5 minutes ahead of the clock of `writ serve`. A `none` row is deleted 25 hours after its key's time.
- `writ apply --remote` waits 30 seconds for each answer. It sends a request to apply at most 5 times, asks the outcome route at most 5 times, and waits 2, 4, 8, and 16 seconds between tries. It follows a run for 30 minutes, counted from when it starts to follow, not from the start of the run.
- An approval of a pipeline covers at most 50 plans.
- An override's reason is 10 to 500 characters, and so is an adoption's.
