---
name: writ
description: Change, plan, ship, and review OpenTofu infrastructure through Writ's control plane. Use when a task edits .tf files or ships an infrastructure change in a repository that Writ plans, reads a plan, run, drift, policy verdict, or what a stack manages, looks up a resource type's attributes, or brings objects that exist outside code under code.
---

# Writ

The infrastructure code here is OpenTofu: `.tf` files, one directory for each stack, each with its `.terraform.lock.hcl`. Writ is the control plane that runs `tofu` for it. Writ plans commits, keeps each plan for people to approve, and applies only an approved plan, with credentials that no person or agent holds.

## Ship a change

"Ship it" means: the change is committed, Writ planned that commit, and you told the user the plan's id. A person approves and applies it.

1. Edit the `.tf` files. Before you write a resource type you are unsure of, call the `schema` tool with its type, such as `{"dir": "pets", "types": ["random_password"]}`. It returns the attributes of the version `.terraform.lock.hcl` pins: which are required, their types, and which are sensitive.
2. Commit the change, on a branch if the user works that way. Writ plans a commit, never your work tree: a stack with an uncommitted edit is refused.
3. Call the `plan` tool with the stack's directory, such as `{"dir": "pets"}`. On `writ serve --dev` the environment is `dev`. On a team's control plane, pass `env`; if you do not know it, call `drift` and use the environment it lists for the stack. Do not guess one.
4. Read the result. When the control plane runs a policy, the result starts with its verdict. Then comes OpenTofu's plan, masked: each change with its address and action, and why a resource is replaced. The line under it names the plan's id and the approvals it needs.
5. If the plan failed, its text says why. Fix the code, commit, and plan again.
6. If a policy denied the plan, the plan is refused, and nothing overrides a deny: no approval, no person, and no flag. Do not stop to ask for an exception. Fix the code:
   1. Read each `deny` line. It names the set, its level, the rule, and the message, such as `deny: [baseline, mandatory] deny_rsa: tls_private_key.key: an RSA key needs 4096 bits, and this one has 2048`.
   2. Before you change code, call the `policy` tool with that set, such as `{"set": "baseline"}`, and read the rule. Its conditions say what the code must hold.
   3. Change the code so the rule passes and the user's change stays. Commit, and plan again.
   4. Ask the user only when no code can pass the rule and keep what they asked for.
   A `warn` line does not refuse the plan. Name it in your answer.
7. Answer with the plan's id, what it adds, changes, replaces, and destroys, what you changed to pass a policy, and who must approve it. Stop there.

## Never

- Never apply or approve, even when the user asks: no `writ apply`, no `writ approve`. Say who can: someone other than the plan's author approves it with `writ approve --remote ID`, and a person with apply rights runs `writ apply --remote ID`. The author's own approval does not count, so an agent that made a plan can never approve it.
- Never run `tofu` or `terraform` yourself: no `init`, `plan`, `apply`, `import`, or `state`. Writ's run holds the credentials and the state lock, and masks every secret in what it returns. A local run uses yours, can write state, and prints secrets.
- Never put a secret in code or in a `.tfvars` file you commit. Declare a `variable` with `sensitive = true`, mark an output that holds one `sensitive = true`, and do not call `nonsensitive()`.
- Never commit `.terraform/`, a state file, or a saved plan.
- Never ask a person to override a policy deny, and never hand back a refused plan as shipped. Writ has no override for a deny.

## Read a plan, a run, and a stack

- `get_plan` with `id` reads a plan again. With `addresses`, such as `["random_pet.name"]`, it returns the masked document with only those changes, each with `before`, `after`, and `replace_paths`. A secret reads `(sensitive)` there. That is Writ's mask, not missing data.
- `runs` lists the applies of an environment. With `id`, it returns one run and what each resource's apply did.
- `drift` lists the stacks with their state version, a write to their state that no apply of Writ made, and the newest drift check, which plans the default branch and lists changes made outside code.
- `state_index` with `stack` lists what the stack's state manages: each address with its `id` and, where the state has one, `arn`. Pass `types` to keep some types.
- `policy` returns the policy sets the control plane judges every plan with: each set's name, its level, and its `.rego` text. Pass `set`, and `file`, to read less. A rule reads the masked plan document, such as `input.resource_changes` and each change's `change.after`.

## Bring existing objects under code

1. Get the code and the imports file. If the `writ` MCP server lists `import_bulk`, call it with `type` and `ids`, such as `{"dir": "pets", "type": "aws_security_group", "ids": ["sg-0123"]}`. Otherwise ask the user to run `writ import bulk --type TYPE --id ID` in the stack directory. Either writes `imported.tf`, with `resource` and `import` blocks, and `imported.imports.json`.
2. Do not edit the generated code yet. Commit both files.
3. Plan the commit with `plan`.
4. Call `import_accept` with the plan's id, such as `{"dir": "pets", "plan": "8b8980230e560dcf5ff00050d7660b6a"}`. It passes when the plan only imports the selected objects and changes nothing else. A refusal names each address and why, such as a change the code makes to an object. Fix the code, commit, plan, and call it again.

## Without the MCP server

The `writ` CLI does the same work against the same control plane, in the stack directory: `writ plan --remote --json` (add `--env ENV` on a team's control plane), `writ show --remote ID --json`, and, with that plan saved as `plan.json`, `writ import accept --imports imported.imports.json plan.json`. `reference.md`, next to this file, lists every tool, its input, and the documents it returns.
