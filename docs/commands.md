# Commands

`writ` is one binary. It runs the control plane, `writ serve`, and the commands you use against one. Plans and applies run on the control plane.

```
writ [--dir DIR] [--env ENV] [--server URL] [--verbose] <command> [flags] [args]
```

## Commands

| Command | What it does |
| --- | --- |
| `writ import accept --imports FILE PLAN` | Accept the plan of an OpenTofu stack after `writ import bulk`: it passes when the plan changes nothing but listed provider-only settings and imports exactly the objects that `FILE`, which `writ import bulk` wrote beside the code, records, each at its address by its import id. `PLAN` is the plan file that `tofu plan -out` saved, which `writ import accept` reads with `tofu show -json` in `FILE`'s directory or `--dir`. A JSON plan, the masked plan document, the output of `writ plan --remote --json`, or the output of `tofu show -json`, passes only imports that plan no update. See [Import and accept](import-bulk.md#import-and-accept). |
| `writ import bulk SELECTION.json` | Take over many existing objects into an OpenTofu stack: write `resource` blocks that declare them, with references in place of literal ids, into one new file, `imported.tf` by default, and the objects it selected into `imported.imports.json`. It writes no state and plans nothing. `--type TYPE --id ID...` selects objects without a file. See [Import many objects](import-bulk.md). |
| `writ agent init` | Write the writ skill for an OpenTofu repository that Writ plans into `.agents/skills/writ` and a copy into `.claude/skills/writ`, and add a section to `AGENTS.md` that points to it. It changes nothing in `AGENTS.md` outside its own section. `--print` writes nothing and prints the files, and `--force` replaces skill files that differ. See [Set up a repository](agents.md#set-up-a-repository). |
| `writ mcp` | Serve Model Context Protocol tools on stdin and stdout for the OpenTofu stacks under `--dir`: `plan` asks the control plane to plan a commit, `get_plan`, `runs`, `drift`, `state_index`, and `policy` read what it keeps, `schema` reads provider schemas from Writ's cache at the versions `.terraform.lock.hcl` pins, and `import_accept` checks an import's plan. No tool applies or approves. `--allow-providers` adds `import_bulk`, which starts the stack's providers with the environment and credentials of `writ mcp`, and lets `schema` download a provider the cache lacks. See [The MCP server](agents.md#the-mcp-server). |
| `writ version` | Print the version. |
| `writ help [COMMAND]` | Print usage for `writ` or for `COMMAND`. |
| `writ serve --config FILE` | Run the control plane. `--check` prints what each team can do and serves nothing, and `--check --notify-test` also sends each notifier a test message. See [Run a control plane](run.md). |
| `writ serve --dev` | Run a control plane for this user alone, on loopback, with its store in SQLite under `$XDG_DATA_HOME/writ/dev`. `--data`, `--listen`, and `--database` change where it keeps its store and where it listens, and `--config FILE` takes `forge`, `repo`, `tofu`, and `policy` blocks and `poll_every`, in HCL. See [writ serve --dev](run.md#writ-serve---dev). |
| `writ plan --remote` | Plan an OpenTofu stack on the control plane, wait, and print the plan the server keeps. In a clean work tree of a [repository](repos.md) the control plane reads, send the commit it's on, which the control plane fetches; with edits, say why no commit can be planned. [How a CLI finds its control plane](sign-in.md#how-a-cli-finds-its-control-plane) gives the server and token it uses. Needs `--env`, except on the dev control plane. |
| `writ plan --remote --stack NAME --commit SHA` | Plan a commit and read no file, as a CI job with no checkout does. `--repo NAME` sets the repository, which a CI job's token already sets. |
| `writ repo add NAME FORGE/PROJECT` | Add a repository: the control plane reads its default branch with the Git host's read token and prints the stacks found. `FORGE` is a forge's name or host. `--path` sets the directory under which it finds stacks, and `--prefix` goes before each stack's name. Needs `admin` on every stack. See [Add a repository](add-repository.md). |
| `writ repo add [NAME] DIR` | Under `writ serve --dev`, add the local Git repository that contains `DIR`, such as `.`. |
| `writ repo list`, `writ repo show NAME` | Print the repositories with the last commit read and the last poll, or one repository and the stacks found at its head. |
| `writ ci gitlab --repo NAME` | Print the `.gitlab-ci.yml` lines that plan each served stack and environment of a repository in merge requests, approve them in a blocking manual job, and apply them after the merge, and the teams, grants, and approval rule the config needs, by the project's id. `--envs`, `--image`, and `--component` change them. |
| `writ ui` | Print the control UI's address. Under `writ serve --dev`, print a sign-in link that works once, for 10 minutes. |
| `writ show --remote ID` | Print a plan `writ serve` keeps, by its id or its ref, or a run, by its id. |
| `writ show --remote --request KEY` | Print the run that the apply request with the key started, or say that none did. A key with no run is closed, so no request with it starts one. `writ apply --remote` prints the key when it can't tell whether its run started. |
| `writ approve --remote ID` | Approve a plan another person created. `--withdraw` takes the approval back until the plan starts applying. See [Approve and apply a plan](apply.md). |
| `writ approve --remote --pipeline` | In a CI job, approve every live plan of the job's own pipeline, all or none. A plan that changes no resource takes no approval, so a pipeline of only such plans passes with nothing approved. |
| `writ apply --remote ID` | Apply a plan `writ serve` keeps, once it has its approvals, and follow the run until it ends. `--override REASON` applies it without them, for a person with `override`. The first Ctrl-C stops following, and the run goes on. Each command sends one key, so its requests start one run at most, and asks whether the run started when an answer is lost. See [When an answer is lost](apply.md#when-an-answer-is-lost). |
| `writ apply --remote --merged --env ENV --stack NAME` | After a merge, apply the stack's approved plan with the same sources as the merged commit: the CI job's own commit on the default branch, or `--repo` and `--commit`. When the newest plan of those sources changes no resource, it says there is nothing to apply and exits 0. |
| `writ apply --remote --yes` | Plan as `writ plan --remote` does, and apply the plan when no approval rule needs one. A plan that changes no resource is printed, and nothing runs. |
| `writ plans --remote` | Print the plans `writ serve` keeps in `--env`, newest first, with what started each: `person` for a person's session, `ci` for a CI job's token. `--stack`, `--status`, and `--started-by` narrow them. While more plans match, the last line names the `--cursor` of the next page. Drift checks aren't plans: the stack's Drift tab and `GET /v1/drift` show them. |
| `writ runs --remote` | Print the runs `writ serve` started in `--env`, newest first, of `--stack` or of every stack. |
| `writ audit --remote` | Print the rows of `writ serve`'s audit log that your token may see, newest first. `--plan ID` takes in the plan's approvals, its run, and the run's state writes; `--run`, `--env` with `--stack`, `--person`, `--op`, `--since`, and `--until` narrow the rows. While more rows match, the last line names the `--cursor` of the next page. See [Read the audit log](api.md#read-the-audit-log). |
| `writ state adopt STACK --env ENV --version N --reason TEXT`, or `--serial N` for a stack on S3 | Seal a secret set with the newest state of an OpenTofu stack that has none, such as a state on S3 that an apply outside Writ wrote, so its plans can be approved again. It needs `adopt`, and a person's session. A secret the state contains where nothing marks it isn't learned. See [Adopt a state](adopt-state.md). |
| `writ login [URL]` | Sign in to a control plane through the team's identity provider, and keep a session, which lasts the control plane's `session_ttl`, 12 hours by default, in `~/.config/writ/sessions.json`, readable by you alone. When the identity provider offers the device flow, it prints a code to type at its page. When it doesn't, as on GitLab.com, it prints a URL to open in a browser on this computer, opens it when it can, and takes the identity provider's answer on a loopback port. `--device` and `--browser` force a flow. With no URL it signs in to the control plane that `--server` or `WRIT_SERVER` sets. `--issuer NAME` picks the identity provider when the control plane has several. See [writ login](sign-in.md#people-writ-login). |
| `writ logout [URL]` | End your session on the control plane and delete it from the session file. |
| `writ whoami` | Print the control plane and how it was found, who the token speaks for and where it came from, its teams, and its grants. It always asks the control plane, and accepts `--remote`. |

`plan`, `apply`, `plans`, `runs`, and `audit` without `--remote` are usage errors that name the remote form.

## Flags

| Flag | Commands | Meaning |
| --- | --- | --- |
| `--json` | every command but `serve`, `mcp`, `ci`, and `help` | Print one JSON document on stdout. With `--help` or `-h`, the usage text prints instead. See [JSON output](json-output.md). |
| `--config FILE`, `--check` | `serve` | Read the HCL server config at `FILE`. `--check` validates the config, compiles policy sets, checks run isolation, lists each notifier, and prints what each team can do without starting the server. |
| `--notify-test` | `serve` | With `--check`, send a test message to each `notify` block's destination, and fail if one doesn't arrive. See [Get notified](notifications.md#check-the-setup). |
| `--dev`, `--data DIR`, `--listen ADDR`, `--database DSN`, `--policy DIR` | `serve` | Run the single-user dev control plane on loopback (`127.0.0.1:8443` by default), storing files under `--data` (`$XDG_DATA_HOME/writ/dev` by default), SQLite or Postgres at `--database`, and optional Rego rules from `--policy DIR` (repeatable, mandatory). |
| `--out FILE` | `import bulk` | The new file to write in the stack, `imported.tf` by default. |
| `--imports FILE` | `import accept PLAN` | The file in which `writ import bulk` recorded the objects it selected, such as `imported.imports.json`. The plan must import exactly those. |
| `--type TYPE`, `--id ID` | `import bulk` | Select objects without a selection file: the type, and an import id to read. Repeat `--id` for each object. |
| `--provider ADDR` | `import bulk` | Read the objects through the provider configuration at `ADDR`. |
| `--max-objects N`, `--max-bytes N` | `import bulk` | Refuse to read more than `N` objects, 500 by default, or to write a file larger than `N` bytes, 1 MiB by default. |
| `--print`, `--force` | `agent init` | `--print` writes nothing and prints the skill files to stdout; `--force` overwrites existing skill files that differ. |
| `--allow-providers` | `mcp` | Enable the `import_bulk` tool (running providers with the caller's environment) and let `schema` download providers missing from the cache. |
| `--yes` | `apply --remote` | Plan as `writ plan --remote` does, and apply the plan when no approval rule needs one. |
| `--merged` | `apply --remote` | Apply the stack's approved plan whose sources match the merged commit (`--commit` or CI commit on the default branch). |
| `--override REASON` | `apply --remote` | Apply a plan without satisfying approval rules, recording `REASON` in the run and audit log. Requires the `override` grant; never bypasses a mandatory policy `deny`. |
| `--withdraw`, `--pipeline` | `approve --remote` | `--withdraw` revokes your earlier approval on a plan before it starts applying; `--pipeline` approves all live plans of the current CI pipeline together. |
| `--request KEY` | `show --remote` | Look up the run started by the idempotency `KEY` (or close `KEY` if no run started). |
| `--version N`, `--serial N`, `--reason TEXT` | `state adopt` | Adopt version `N` (on the `http` backend) or serial `N` (on `backend "s3"`) of a stack's state and record `REASON` in the audit log. |
| `--device`, `--browser`, `--issuer NAME` | `login` | Force the OAuth 2.0 device flow (`--device`) or authorization-code + PKCE loopback flow (`--browser`), or select the configured identity provider `--issuer NAME`. |
| `--path DIR`, `--prefix PREFIX` | `repo add` | Discover stacks under `DIR` inside the repository and prepend `PREFIX/` to each stack's name. |
| `--repo NAME`, `--envs a,b`, `--image IMAGE`, `--component PATH` | `ci gitlab` | Generate `.gitlab-ci.yml` for repository `NAME`, overriding the comma-separated environment list (`--envs`), container image (`--image`), or GitLab CI component path (`--component`). |
| `--remote` | `plan`, `apply`, `approve`, `show`, `plans`, `runs`, `audit`, `state`, `whoami` | Run against `writ serve`. `state` and `whoami` always do. See [How the control plane works](serve.md). |
| `--limit N` | `plans --remote`, `runs --remote`, `audit --remote` | Print the newest `N` plans, runs, or rows, from 1 to 200, 50 by default. |
| `--status STATUS` | `plans --remote` | Print only the plans of the status, as `GET /v1/plans/{id}` shows it, such as `pending`, `applied`, or `no-changes`. |
| `--started-by person`, `--started-by ci` | `plans --remote` | Print only the plans a person started from a session, or the plans of a CI job's token. |
| `--plan ID`, `--run ID`, `--person ID`, `--op OP` | `audit --remote` | Print only the rows of the plan, by its id or ref, with its run's; of the run; of the person, as `writ whoami` prints the id; or of the operation. |
| `--since TIME`, `--until TIME` | `audit --remote` | Print only the rows at or after `--since`, and before `--until`, both RFC 3339 times such as `2026-11-19T09:30:00Z`. |
| `--cursor C` | `plans --remote`, `audit --remote` | Print the page after the one that named `C`. |
| `--stack NAME` | `plan`, `apply`, `plans`, `runs`, `audit` with `--remote` | The stack's name on `writ serve`. For `plan` and `apply`, the default is its path from the Git work tree, or its directory's name outside one. `plans` and `runs` list only that stack's plans or runs when it's given, and every stack's in the environment when it isn't. `audit` needs `--env` with it. |
| `--hint-merge-request N` | `plan` with `--remote` | The merge request the plan belongs to, which `writ serve` keeps only for a merge request pipeline. |
| `--commit SHA`, `--repo NAME` | `plan` and `apply` with `--remote` | Plan this commit of the repository, and read no file. A CI job's token sets its repository and its commit, and `writ serve` refuses another. |

These global flags work before or after the command:

| Flag | Meaning |
| --- | --- |
| `--dir DIR` | The stack directory. The default is the current directory. |
| `--env ENV` | The environment of the control plane to run in. `WRIT_ENV` sets the same thing. |
| `--server URL` | Run on the control plane at `URL`, or on the dev control plane with `--server dev`. It implies `--remote` and comes before `WRIT_SERVER` and your session. |
| `--verbose` | Log at debug level, to stderr. |

## Output for people

Without `--json`, `writ plan --remote` and `writ apply --remote` print the plan and the apply as the control plane writes them, for a person reading a terminal or a CI log.

- **A plan lists each change of the masked plan,** one line per resource with its address and action (`+` create, `~` update, `-` delete, `-/+` replace with the paths that force it, `<-` import), then OpenTofu's summary line, the outputs that change, the policy decision, and any warnings.
- **An apply prints when its run ends:** each resource's outcome and the run's summary. `writ show --remote RUN` prints it again later.
- **Plain text.** The plan and apply text is printed as the control plane wrote it, without color, and a value in it never prints raw escape codes.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | The command succeeded. |
| 1 | The command failed: a diagnostic, a refused or failed plan or apply, a request that `writ serve` refused, or an error that stopped it. |
| 2 | The command line or the command's input is wrong: an unknown flag, a missing argument, a directory that isn't an OpenTofu stack, or no control plane to send it to. It doesn't mean nothing changed: `writ agent init` can write `AGENTS.md` before it exits 2. |
| 130 | Interrupted by `SIGINT` or `SIGTERM` (for example, pressing Ctrl-C). |

With `--json`, stdout has one JSON document on exit codes 0, 1, and 2.

## Environment variables

| Variable | Meaning |
| --- | --- |
| `WRIT_ENV` | The environment to run in. The same as `--env`. |
| `WRIT_SERVER` | The control plane the remote commands use, after `--server`. |
| `WRIT_ID_TOKEN` | The CI job's OIDC token the remote commands send. It comes before a session from `writ login`. |
| `XDG_CONFIG_HOME` | Where `writ login` keeps its sessions, in `writ/sessions.json`. The default is `~/.config`. |
| `XDG_DATA_HOME` | Where `writ serve --dev` keeps its data, under `writ/dev`, and where the remote commands look for its `client.json`. The default is `~/.local/share`. |
| `WRIT_CACHE_DIR` | The directory for provider binaries and cached schemas, which `writ import bulk` fills and the `schema` tool of `writ mcp` reads. |
| `WRIT_CPUPROFILE` | Write a Go pprof CPU profile of the `writ` invocation to this file path on exit. |
| `PGPASSFILE` | Read by `writ serve` when connecting to Postgres if `database` omits a password; defaults to `~/.pgpass`. |
| `DISPLAY`, `WAYLAND_DISPLAY` | Checked by `writ login` on Linux before attempting to launch a browser with `xdg-open`. |
| `USER` | Used by `writ serve --dev` as the local display name (`dev/user/$USER`, or `local` if unset). |
| `NO_COLOR` | A value that isn't empty turns off color on a terminal. See [Output for people](#output-for-people). |

`writ import bulk` and the `import_bulk` tool of `writ mcp` run providers with the environment `writ` runs in. A provider's own settings, such as `AWS_PROFILE`, work.
