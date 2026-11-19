# Secrets and the mask

When you run `tofu plan` and `tofu apply`, they print secrets. Your plan's JSON has the values of your sensitive variables, and an error message can quote one. If a control plane kept that output, anyone who could read a plan could read your secrets.

So `writ serve` keeps none of that raw output. Instead, it keeps your run's secrets out of everything it stores and shows by passing all of it through a mask.

This page covers how the mask works, how `just verify-mask` tests it, and which cases it can't catch. Read the last part before you trust the mask with a stack.

## How the mask works

One package, `internal/mask`, produces every document that leaves a run:

- the masked plan document, which the API, the control UI, and `writ plan --remote` show,
- the text of a plan and of an apply, and the events of an apply,
- the rows of the [state index](serve.md#the-state-index),
- the diagnostics of a plan or an apply that failed.

Your runs each get their own mask. At first, it knows the values of the environment that `writ serve` gave the run, such as your `provider_env` and the `TF_VAR_` variables, and learns more from each document it reads. Then it applies four rules.

When the run has an AWS role, the mask also knows the secret access key and session token of every set of credentials `writ serve` assumed for it, including each refresh during a long run. Your providers fetch those from the [credential endpoint](serve.md#roles-and-credentials), so they're not in the run's environment, but a provider error can quote them. The mask checks for new credentials before each document it sweeps. The access key id stays visible, because it's no secret and it's how you find the session in CloudTrail. These credentials are never sealed into a state's secret set.

### Rule 1: OpenTofu's marks

When a value is under a sensitive mark, before or after, it becomes `(sensitive)`. The mask also learns the value, and the values of every variable you declared `sensitive = true`.

From these it learns strings, and numbers of six or more characters. If you have a boolean, or a number shorter than six characters, the mask hides it only where it's marked.

When a marked value changes, your document lists its path. So as a reviewer, you see that a secret changed without seeing it.

### Rule 2: a sweep by value

Next, the mask searches every string, key, and number in the document for each value it has learned. Beyond the plain value, it looks for the encodings it decodes: URL escapes, JSON escapes, hex, and base64. That's how it catches a secret that `nonsensitive()` removed the mark from, such as `nonsensitive(base64encode(var.token))`.

If a learned string is shorter than six bytes, the mask replaces it only where it's a whole value, not inside a longer string. Otherwise a secret such as `1` would blank the document.

The sweep skips what your configuration sets and no value can set: the format and OpenTofu versions, the names of variables and outputs, the providers and modules of the configuration, and for each resource change its type, name, mode, provider, module, actions, and action reason, the address apart from its index keys, and the first step of each replace path. So a secret that spells `random_id` or `delete` leaves a resource's type or actions whole, and a policy or a risk class reads them as OpenTofu wrote them.

Index keys are different, because a value sets them, so the sweep covers them. To do that, the mask parses an address as OpenTofu does and sweeps each key as the value it decodes to. That way it still finds a key OpenTofu wrote with an escape such as `$${`. A swept key is written `["(sensitive)"]`, and if an address doesn't parse, the mask sweeps it whole.

### Rule 3: names

If an attribute or key has a name that contains a word such as `password`, `secret`, or `token`, its value becomes `(sensitive)`, whatever its marks say.

A name that sets the tokens an auth method issues is the one exception for `token`: one `token` followed by `ttl`, `max_ttl`, `explicit_max_ttl`, `period`, `num_uses`, `locality`, `name_format`, `type`, `policies`, `bound_cidrs`, or `no_default_policy` at the end of the name, such as `max_token_ttl`, `token_locality`, or `token_policies`. Such a value still goes through rules 1 and 2, so a marked or learned secret in it is still masked. `api_token`, `token_value`, and `tokenless` stay masked by name.

### Rule 4: an allow-list of keys

Your masked plan document keeps only the keys the mask lists. You'll never see a key it doesn't list in the document.

### When the mask keeps less

Two limits fail closed, so you lose the document rather than risk a secret. If your run's secrets pass 4 MiB, or one string's decoding and matching costs more than 32 MiB, Writ stores no document at all for that run. The 4 MiB counts each distinct secret once, the role credentials included.

If your plan fails before the mask has read it, Writ keeps only each diagnostic's severity, summary, file, line, and column, swept. Writ never keeps a diagnostic's detail or snippet, because a validation's `error_message` can quote a variable through `nonsensitive()`. On a stack whose state is on S3, each instance key a summary quotes once or twice also becomes `["(sensitive)"]`, as [What a run can see](serve.md#what-a-run-can-see) explains.

Nothing else in Writ reads your run's raw state. To enforce that, `internal/archtest` refuses any code outside `internal/tofu` that reads it, other than as the argument of the mask's `Index`.

## What the mask can't do

Think of the mask as a filter on output. So it finds a secret only when OpenTofu marks it, when it contains a value the mask learned, or when the attribute's name says so. As above, it catches a short secret only as a whole value, and a boolean or a short number only under its mark.

These cases stay open, so you need to know them:

- **An unmarked secret in an unmarked attribute.** If nobody declared a variable `sensitive`, and you write it to an attribute that the provider doesn't mark, the mask sees plain text, in the plan and in state.
- **A transformed secret passed to `nonsensitive()`.** `nonsensitive(sha256(var.token))` prints a value that the mask never learned and can't derive. The sweep undoes encodings, not hashes or other functions.
- **Base64 broken into lines.** The sweep decodes each line of base64 separately. Say a secret of six or more bytes has base64 that crosses a line break, as a PEM body breaks every 64 characters. Unless its value is marked, the mask doesn't find it, even in a string that contains nothing else. If its base64 lies within a line, the mask finds it. A secret shorter than six bytes is found only where the whole string decodes to it, and there with or without line breaks.
- **A number written another way.** The sweep finds a learned number by its text, also inside a longer text, so `150000` is found in `150000.0`. Written without that text, such as `1.5e5`, `1.5E+05`, or `150,000`, the same number isn't found.
- **The old value of an attribute that `nonsensitive()` unmarked.** In an update, the mask sweeps the `before` value of such an attribute only for the secrets it knows: the state's secret set, the values the plan marks, and the run's environment. If none of these contains an old secret, that secret shows.
- **A secret a provider returns unmarked after apply.** Your provider can return a secret in an attribute it doesn't mark. That value is then in your state, which `writ serve` seals under the environment's key. Because the state index keeps only an allow-list of attributes, such a secret reaches the index only when the provider puts it in one of those attributes, such as `id`.

To stay clear of these cases, declare every secret variable `sensitive = true`, and don't call `nonsensitive()` on a value derived from one.

## How it's tested

To test the mask, run `just verify-mask`, a differential test. This runs the real path, `writ serve --dev` with OpenTofu 1.13.9, in user, network, mount, and pid namespaces where only loopback exists. You can choose how many stacks it generates and how long it fuzzes:

```
just verify-mask            # 6 generated stacks, then 30 s of fuzzing
just verify-mask 20 2m      # 20 stacks, 2 minutes of fuzzing
```

First, a generator writes OpenTofu stacks over `hashicorp/random`, `local`, and `tls`. In each stack, a sensitive variable passes through some of these: a direct reference, a string template, `base64encode`, `urlencode`, `jsonencode`, and `sha256`, a nested block, `for_each`, a module, outputs, a `for_each` key made with `nonsensitive()`, and `nonsensitive()` of the plain and the encoded value.

Then each stack is planned and applied twice, in two control planes. Those two runs differ only in the variable's value, which is one of two canary strings. Three checks compare them:

- **Check 1.** Everything the API serves, every plan, run, and list, and every row of the control database apart from the sealed blobs, is the same in both runs once ids, hashes, and times are normalized. If a canary reached any of it, the two runs would differ.
- **Check 2.** Neither canary, plain, base64, hex, or URL-encoded, is anywhere in the control database, the data directory, the scratch directory, an output of a command, or the server's log.
- **Check 3.** Each control plane is restarted with its state index emptied, so it indexes its stacks again from the stored state. The default of a sensitive variable, which only the stack's files and its plan contain, and which a `for_each` key exposes through `nonsensitive()`, is in no row of the index, no output, and no log line.

Finally, `just verify-mask` runs `FuzzPlan`, a Go fuzz test that feeds the mask arbitrary JSON with arbitrary marks. Whenever the mask accepts a plan, no value under a mark and no canary may survive anywhere in the masked document, plain or encoded.

**Limits.** Because the test configures no policy and renders no page of the control UI, its checks don't cover a policy's messages or the pages. The control UI's pages read only the masked document. Writ sends no notification.

The mask is tested. It is not guaranteed.

### Where to look

| Claim | Where |
| --- | --- |
| One package produces every document that leaves a run | `internal/mask/` |
| No other code reads a run's raw state | `internal/archtest/state_test.go` |
| The differential test, Checks 1 to 3 | `e2e/verify_mask_test.go`, `justfile` (`verify-mask`) |
| The fuzz test | `internal/mask/fuzz_test.go` |

## Next steps

- [OpenTofu stacks](serve.md#opentofu-stacks): how a plan of an OpenTofu stack runs, and its filesystem view.
- [How an OpenTofu plan is approved and applied](approvals.md#how-an-opentofu-plan-is-approved-and-applied): what the masked plan document contains, and how it's approved.
