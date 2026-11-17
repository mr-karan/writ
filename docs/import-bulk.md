# Import many objects

`writ import bulk` brings objects that someone created by hand, in a console or with a script, under an OpenTofu stack. Writing a `resource` and an `import` block for each one is slow and easy to get wrong, so it writes them for many objects in one run.

It reads your objects from their providers and writes one new file of `resource` and `import` blocks that declare them. Then `tofu plan` plans their import, and `writ import accept` checks that plan:

```
writ import bulk SELECTION.json [--out FILE] [--max-objects N] [--max-bytes N] [--json]
writ import bulk --type TYPE --id ID... [--provider ADDR] [--out FILE] [--max-objects N] [--max-bytes N] [--json]
writ import accept --imports FILE PLAN [--json]
```

## How it works

Run `writ import bulk` in a directory that has a `.terraform.lock.hcl`, `.tf` or `.tofu` files, and no `.writ` file; it refuses any other. From there it reads your objects through their providers, and writes `resource` blocks and `import` blocks into one new file, `imported.tf` by default.

Beside that file, in `imported.imports.json` (the name of `--out` with `.imports.json` in place of `.tf`), it records each object it selected: the resource address the import block adopts it as, and its import id. You get no state written and nothing planned.

Your `tofu plan` reads the objects again and adopts them. Writ accepts the import when that plan imports exactly the recorded objects and changes nothing but the provider-only settings below.

Here's what to expect from the run:

- **Your lock file decides the providers.** Writ reads each object through the version the lock pins, and starts it only when the archive in Writ's provider cache has one of the lock's `zh:` hashes. If the cache lacks a provider, Writ downloads it first.
- **Writ reads your code from regular files.** It reads the stack's code from its regular `.tf` and `.tofu` files. If your stack has a `.tf.json` or `.tofu.json` file, or a `.tf` or `.tofu` file that's a symlink, Writ refuses it, because OpenTofu reads those too, and a name they declare could clash with the code Writ writes.
- **Your provider blocks configure the providers.** Writ reads literal values only: if an argument refers to a variable or calls a function, the run is refused before any read. Set such a value in the environment the provider reads.
- **You name types as OpenTofu does.** A selection lists types such as `random_uuid`, and a provider configuration as `NAME.ALIAS`, such as `aws.west`.
- **Objects of one shape share a resource.** They become one `resource` with `for_each`, and one `import` block over the same keys. A nested block takes no expression, so objects whose nested blocks differ don't share a resource.
- **Resources get readable names.** Writ never puts an ARN or an account id in a resource name or a `for_each` key. See [How resources are named](#how-resources-are-named).
- **Secrets become variables.** A secret becomes a `variable` with `sensitive = true`. Set it with `TF_VAR_NAME`.
- **Some objects can't be imported safely.** OpenTofu refuses an import id that's sensitive, and prints every import id in its plan. So if an object's import id is one of its own secrets, such as a `random_password`, it's `not imported`, and nothing is written.
- **Runtime-owned fields are ignored.** A field that a runtime usually owns goes into `lifecycle { ignore_changes = [...] }`.
- **Long strings are heredocs.** A string of several lines, such as a Nomad jobspec, is a heredoc.
- **Strings must be NFC.** OpenTofu keeps every string of its code in Unicode normal form C (NFC), so an id of `e` and a combining accent would import the precomposed `é`, and tofu would plan to replace the object. If an object's import id, or any string or map key of its code, is in another form, it's `not imported`, and nothing is written.
- **`--out` must be a new file.** It sets a new `.tf` file. Writ refuses it when it or its `.imports.json` file exists, or when `X.tofu`, `X.tf.json`, or `X.tofu.json` exists for `--out X.tf`: OpenTofu ignores `X.tf` beside `X.tofu`, and reads the JSON forms with it. Writ also refuses `override.tf` and a name that ends in `_override.tf`, which OpenTofu reads as an override of other files.

### How resources are named

Writ names each resource after what you'd call the object: its `name` attribute, else its `Name` tag, else its import id. A `for_each` key comes from the same places, when every object in the resource has a different one.

Some types have an ARN for an import id and no `name` attribute. An ARN contains your account id, so Writ builds their names from the object's settings instead, under its parent's name when the run imports the parent too:

| Type | Built from | Example |
| --- | --- | --- |
| `aws_lb_listener` | load balancer, protocol, port | `aws_lb_listener_web_http_80` |
| `aws_lb_listener_rule` | listener, then the `Name` tag or the priority | `aws_lb_listener_rule_web_http_80_priority_10` |
| `aws_acm_certificate` | domain name | `aws_acm_certificate_example_com` |

Without its load balancer in the run, that listener is `aws_lb_listener_http_80`. Writ never uses a label that contains `arn:`, or a run of exactly 12 digits, as an AWS account id is, in a name or a `for_each` key. The one exception is the last group of a whole UUID: 36 characters of hex digits in groups of 8, 4, 4, 4 and 12, with no letter or digit right before or after them. A UUID that a letter or digit touches gets no exception. An object with nothing else to go by gets a number, such as `aws_acm_certificate_1`.

## Import and accept

Here's a run with a selection file of random objects that `tofu apply` created in another stack:

```
$ writ import bulk select.json
+ random_uuid.random_uuid["378c806e-ce8f-66fe-532b-b6ff92fd129f"]                 random_uuid     "378c806e-ce8f-66fe-532b-b6ff92fd129f"
+ random_uuid.random_uuid["6fb9213d-99f8-d9d1-4063-65beff531ce0"]                 random_uuid     "6fb9213d-99f8-d9d1-4063-65beff531ce0"
+ random_integer.random_integer["24,1,100,6fb9213d-99f8-d9d1-4063-65beff531ce0"]  random_integer  "24,1,100,6fb9213d-99f8-d9d1-4063-65beff531ce0"
+ random_integer.random_integer["93,1,100,378c806e-ce8f-66fe-532b-b6ff92fd129f"]  random_integer  "93,1,100,378c806e-ce8f-66fe-532b-b6ff92fd129f"
+ random_string.random_string_x4yq6cmdasfw                                        random_string   "x4yq6cMdaSFW"
wrote imported.tf: 2 resources with for_each, 1 single resource, 2 references in place of literals, 0 typed references left as literals
wrote imported.imports.json: the objects that tofu plan should import
Read 5 objects and wrote no state. tofu plan should show 5 to import, 0 to add, 0 to change, 0 to destroy; save it with tofu plan -out=PLAN and check it with writ import accept --imports imported.imports.json PLAN.
```

Part of what `imported.tf` has:

```hcl
# 2 random_integer objects
resource "random_integer" "random_integer" {
  for_each = {
    "24,1,100,6fb9213d-99f8-d9d1-4063-65beff531ce0" = {
      random_uuid = "6fb9213d-99f8-d9d1-4063-65beff531ce0"
    }
    "93,1,100,378c806e-ce8f-66fe-532b-b6ff92fd129f" = {
      random_uuid = "378c806e-ce8f-66fe-532b-b6ff92fd129f"
    }
  }
  max  = 100
  min  = 1
  seed = random_uuid.random_uuid[each.value.random_uuid].id
}

import {
  for_each = toset(["24,1,100,6fb9213d-99f8-d9d1-4063-65beff531ce0", "93,1,100,378c806e-ce8f-66fe-532b-b6ff92fd129f"])
  to       = random_integer.random_integer[each.key]
  id       = each.key
}
```

And part of what `imported.imports.json` has:

```json
{
  "format": "writ.import-selected/v0",
  "imports": [
    {
      "type": "random_integer",
      "name": "random_integer",
      "key": "24,1,100,6fb9213d-99f8-d9d1-4063-65beff531ce0",
      "id": "24,1,100,6fb9213d-99f8-d9d1-4063-65beff531ce0"
    },
    {
      "type": "random_string",
      "name": "random_string_x4yq6cmdasfw",
      "key": null,
      "id": "x4yq6cMdaSFW"
    }
  ]
}
```

In that file, each import refers to a resource of the root module by `type`, `name`, and `key`, the `for_each` key, `null` for a resource without `for_each`, and `id` is its import id. Keep the file next to your code until the import is accepted.

In the stack's empty state, `tofu plan` printed `Plan: 5 to import, 0 to add, 0 to change, 0 to destroy.` Then save the plan and check it:

```
$ tofu plan -out import.plan
$ writ import accept --imports imported.imports.json import.plan
Accepted: the plan imports 5 objects and changes nothing else.
```

Here `PLAN` is the plan file that `tofu plan -out` saved.

### How writ import accept reads the plan

When you run `writ import accept`, it reads `PLAN` with `tofu show -json PLAN`, which it runs itself, with the `tofu` on `PATH`. That runs in the directory of the imports file, or in `--dir` when that isn't `.`. That directory must be your stack's, initialized, because `tofu show` reads the providers' schemas from its `.terraform`.

If `tofu show` fails, `writ import accept` fails without printing what `tofu` wrote to stderr, which the mask hasn't read. To see why, run the same command there yourself.

You can also pass a JSON document of the plan as `PLAN`, or `-` to read one from stdin: the masked plan document, the output of `writ plan --remote --json`, or the output of `tofu show -json`, which it masks first. Nothing proves that a JSON document shows the plan that will be applied. So from one, `writ import accept` accepts only imports that plan no update, and it refuses an import update with a reason that tells you to pass the saved plan file.

### What it accepts

Writ accepts your plan when all of these are true:

- Your plan didn't error.
- It imports each object that `--imports` records exactly once, at its address and by its import id, as a no-op or as an update of provider-only settings alone.
- It imports nothing else.
- Every other resource change is a no-op. If a no-op imports nothing, it's a resource your state already has.

You get a refusal, with the address, for any of these: an import at another address, an import in a module, an import whose `address` isn't the one its `type`, `name`, and `index` make, a second import at one address, an import with another id, a recorded object the plan doesn't import, and any other change.

### Provider-only settings

Some providers keep a setting that the object doesn't have, and OpenTofu plans it in place on the first plan after the import. So `writ import accept` has a fixed list of such provider-only settings, each measured on the provider and version it lists:

| Provider | Version | Type | Settings |
| --- | --- | --- | --- |
| `registry.opentofu.org/hashicorp/nomad` | 2.6.1 | `nomad_job` | `deregister_on_destroy`, `deregister_on_id_change`, `detach`, `preserve_counts`, `read_allocation_ids`, `rerun_if_dead` |

For example, if you import a Nomad job that `tofu` registered with `hashicorp/nomad` 2.6.1, the plan is `1 to import, 0 to add, 1 to change`, and the update sets each of those six settings from null to its default (measured with `just e2e-import-hcl` on 2026-11-14). Since the provider gives each setting a default and its importer sets none of them, your code that sets the defaults plans the same update.

Writ accepts an import that updates in place, with no replace path, when every attribute that the update changes is on the list for its provider and type, and the value that each sets is known and not sensitive. You see each setting printed:

```
$ writ import accept --imports imported.imports.json import.plan
~ nomad_job.nomad_job_import_web_default: sets deregister_on_destroy = true, a setting that registry.opentofu.org/hashicorp/nomad 2.6.1 keeps and the cloud does not return
~ nomad_job.nomad_job_import_web_default: sets deregister_on_id_change = true, a setting that registry.opentofu.org/hashicorp/nomad 2.6.1 keeps and the cloud does not return
~ nomad_job.nomad_job_import_web_default: sets detach = true, a setting that registry.opentofu.org/hashicorp/nomad 2.6.1 keeps and the cloud does not return
~ nomad_job.nomad_job_import_web_default: sets preserve_counts = false, a setting that registry.opentofu.org/hashicorp/nomad 2.6.1 keeps and the cloud does not return
~ nomad_job.nomad_job_import_web_default: sets read_allocation_ids = false, a setting that registry.opentofu.org/hashicorp/nomad 2.6.1 keeps and the cloud does not return
~ nomad_job.nomad_job_import_web_default: sets rerun_if_dead = false, a setting that registry.opentofu.org/hashicorp/nomad 2.6.1 keeps and the cloud does not return
Accepted: the plan imports 1 object, sets 6 provider-only settings that the cloud does not return, and changes nothing else.
```

With `--json`, the report's `settings` lists each setting with its `address`, `attribute`, `value`, `provider`, and `version`.

Writ refuses an update that also changes another attribute, such as the jobspec, adds or removes one, even as null, changes its sensitive marks, or makes a value unknown until apply. You'll see each attribute that isn't on the list in the refusal. Writ also refuses an update of a listed setting whose value is unknown until apply, or sensitive, anywhere in it, and the refusal gives the setting.

To see what the update changes, `writ import accept` reads the values of `tofu show -json` of the saved plan before it masks them. That's because the mask can replace two different values, such as two jobspecs that sensitive variables contain, with one placeholder. Everything it prints, values, attribute names, and refusals, passes the mask of that plan, so if an attribute name contains a sensitive value, you see `(sensitive)` in its place.

Only from the saved plan file is an import that plans an update accepted. Because the list matches the provider's address in the plan, a fork of a provider isn't on it. Writ doesn't check the provider's version, because the plan has only the version constraint.

When you apply the plan, the settings are stored in state. For Nomad, the `hashicorp/nomad` update registers the jobspec again, and the Nomad agent keeps the job's version and modify indexes because the spec didn't change. Your next plan has no changes (measured the same way). Note that `writ import bulk` doesn't use the list yet: for such a job it reports that the provider couldn't prove a no-change plan, and still expects `0 to change`.

Launch templates aren't on the list. With `hashicorp/aws` 6.70.0, a launch template's import reads `update_default_version` as null and `writ import bulk` leaves it out, so the plan only imports. If your code sets it to `true` instead of `default_version`, the plan has `default_version` as unknown, because the provider's update moves the default version to the latest one. That's a change to the object, and `writ import accept` refuses it (measured against a stand-in of the AWS API with `just e2e-import-hcl` on 2026-11-14).

## Write a selection file

You write a selection file in JSON, because an id can contain commas, quotes, and newlines. Pass `-` to read it from stdin. In it you list entries under `objects`, and each entry selects objects of one `type` in one of four ways:

| Field | Selects |
| --- | --- |
| `ids` | The objects with these import ids. |
| `values` and `id` | One object for each record in `values`. `id` is a template: `{b}` is the record's field `b`. |
| `from` and `id`, with `over` | One object for each object of type `from` that an earlier entry read. `{parent.ATTR}` reads that object. With `over: "route"`, one object for each element of the parent's `route` list, and `{item.ATTR}` reads the element. |
| `list` | The objects that the provider lists, with the record as the list config of the type. See [Listing objects](#listing-objects). |

Keep these rules in mind:

- Set `type` as OpenTofu does, such as `aws_subnet`. To read an entry's objects through another provider configuration, set `provider` on the entry, written `NAME.ALIAS`, such as `aws.west`, and the code sets `provider`.
- In a template, `{{` and `}}` write a brace. If a placeholder's value is null or missing, Writ skips that one object. If a placeholder's value is a secret, Writ refuses it.
- Your template can be at most 2,048 bytes, and so can the id it builds. Your run expands templates at most 10 times `--max-objects`, counting the expansions skipped for a missing value.
- Entries run in order, so `from` refers to a type that an earlier entry selects.

For a few objects, you don't need a file. With `--type TYPE --id ID`, you select objects the same as one entry with `ids`. Repeat `--id` for each object.

## Listing objects

With a `list` entry, you ask the provider for the objects of a type through `ListResource`, a call of plugin protocol versions 5.10 and 6.10. Your record is the type's list config, which the provider defines separately from the resource's attributes.

In `hashicorp/aws` 6.70.0, for example, the list config of `aws_subnet` has `region`, `subnet_ids`, and a `filter` block, and no `vpc_id`. This entry selects the subnets of one VPC:

```json
{
  "objects": [
    { "type": "aws_subnet", "list": { "filter": [{ "name": "vpc-id", "values": ["vpc-00000000000000001"] }] } }
  ]
}
```

Here's how a listing behaves:

- If you give a field that the list config doesn't have, Writ refuses the entry before it calls the provider, and the reason gives the field. Writ then asks the provider to validate the query with `ValidateListResourceConfig`, and an error refuses the entry before the listing, such as a `filter` without its required `name`.
- The provider returns a resource identity for each object. For the import id, Writ uses the one identity attribute that the provider requires for import, such as `id` for `aws_subnet` and `aws_instance`, `name` for `aws_iam_role`, and `bucket` for `aws_s3_bucket`. Writ then reads each id as an `ids` entry does, with the same refusals and skips. If an earlier entry selected an object, Writ skips it.
- `--max-objects` bounds a listing. Only ids that no earlier entry selected count against it. Writ asks the provider for one more object than the room left, plus one for each id the run already selected of the type, and stops reading at that count. If the provider has more new objects than the room, the run stops and writes nothing.
- A provider can list one object more than once. So Writ asks for 4 times as many objects as it needs plus 16, and keeps each id once. If the provider sends that many before Writ has the distinct ids it needs, the list may have more objects than it showed, so the entry is `not imported`.
- A listing keeps at most 20 warnings and 16 KiB of their text and attribute paths, cuts each path to 1 KiB, and counts the rest. If an identity is larger than 16 KiB, or an import id longer than 2,048 bytes, the entry fails.
- If the provider returns an error for any object, the whole entry fails, so a part of a list never passes for all of it.
- A secret in your query is a secret of the run, so no problem prints it. When the query contains a secret, Writ withholds the text of a refusal of the listing, because a provider may quote or escape the secret it echoes.
- Writ reads each id through the entry's provider configuration. For the objects of another region, set `provider` on the entry to a configuration for that region.

Some types can't be listed. You get `not imported`, with a reason, when the type's provider has no list resource for it, or when its identity has more than one attribute required for import, or none.

In `hashicorp/aws` 6.70.0, 259 types have a list resource, and 179 of them have exactly one identity attribute required for import, a string. Those other 80 include `aws_ecs_service`, whose identity is `cluster` and `name`, and `aws_route53_record`. We counted them in a scratch run from the provider's `GetProviderSchema` and `GetResourceIdentitySchemas` answers on 2026-11-12. Neither `random` 3.9.1, `tls` 4.4.2, nor `local` 2.9.1 has a list resource.

For example, this problem is from the JSON report of a `list` entry for `random_uuid`:

```json
{"kind":"not imported","from":"objects[0]","reason":"provider hashicorp/random does not list objects of random_uuid; select them by ids, values, or from"}
```

## Refusals and skips

| Case | Result |
| --- | --- |
| The file set by `--out` exists. | A `usage` error before any read. |
| The file set by `--out` appears while the objects are read. | A `usage` error. The file is kept, and nothing is written. |
| An id is empty or longer than 2,048 bytes. | A `usage` error before any read. |
| The provider finds no object, or rejects the id. | The object is `not imported`, and nothing is written. |
| The templates expand more than 10 times `--max-objects`. | A `usage` error, and nothing is written. |
| An `ids` or `values` entry selects an object that an earlier id selected. | A `usage` error that names both entries, and nothing is written. One object is the same type, provider configuration, and import id, or two import ids that the provider reads as the same object. |
| A `from` entry derives an id for an object that an earlier id selected. | The object is `skipped`, because the run imports it once. |
| A placeholder has no value, or `from` refers to a type the run read no object of. | That object, or that entry, is `skipped`. |
| No object is left to import. | A `usage` error, and nothing is written. |

## Limits

- `--max-objects N`, 500 by default, bounds the objects that one run reads. That count includes the objects that the entries expand to, and the ids whose reads fail. Your run counts each entry's ids before it reads any of them. When the count passes the limit, the run stops and writes nothing.
- `--max-bytes N`, 1 MiB by default, bounds the file after formatting. A larger file is refused, and nothing is written.
- A selection file is at most 4 MiB.
- The secrets that the check compares values against are at most 64 MiB, counting each distinct secret of 8 or more bytes once, without its line breaks. Their index uses about 5 bytes of memory for each byte of secret. If they contain private keys, their key text has a second index, of the same size or smaller. When the limit is passed, the run stops before it builds an index, and writes nothing.
- The time that the check takes grows with the size of each value that it compares. When a secret contains a PEM private key, a value of 16 MiB of random base64 took 2.2 s to check, and 0.15 s when no secret contains a key. We measured both with a `go test` run on 2026-11-12, on a 16-core x86-64 machine. Writ has no limit of its own on the size of a value that the check compares. A provider sends at most 64 MiB in one message, so one value is at most 64 MiB.
- Reads run at most 10 at a time.

## What it doesn't do

- It doesn't import by resource identity. If you write a `list` entry for a type whose identity has more than one attribute required for import, it's `not imported`. Select those objects by `ids`, `values`, or `from`.
- It doesn't keep the object that a provider lists. It reads each listed id again, so a listing of 50 objects is one list call and 50 reads.
- It doesn't derive ids from resources that the stack had before the run. `from` reads objects of the same run.
- It doesn't group objects whose attributes differ in shape.
- It can't read a type whose provider refuses import, such as `local_file` and every `tls` type.

## JSON

With `--json`, you get one `writ.import-bulk/v0` document, and from `writ import accept --json` one `writ.import-accept/v0` document. [JSON output](json-output.md) gives their fields.

## Next steps

- [Commands](commands.md): every command and flag.
- [How the control plane works](serve.md): plan and apply the stack on the control plane.
