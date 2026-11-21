# Import many objects

`writ import bulk` reads existing cloud objects through OpenTofu providers and writes a `.tf` file of `resource` and `import` blocks, plus a manifest of the selected objects. Save a plan with `tofu plan -out=PLAN` and check it with `writ import accept` before you apply:

```sh
writ import bulk SELECTION.json [--out FILE] [--max-objects N] [--max-bytes N] [--json]
writ import bulk --type TYPE --id ID... [--provider ADDR] [--out FILE] [--max-objects N] [--max-bytes N] [--json]
writ import accept --imports FILE PLAN [--json]
```

## How it works

Run it in a stack directory with `.terraform.lock.hcl`, `.tf` or `.tofu` files, and no `.writ` file. It writes `imported.tf` (or `--out`) and a manifest, `imported.imports.json` (the `--out` name with `.imports.json` for `.tf`). It neither plans nor writes state.

- **Lock-file verified providers:** Runs the exact provider versions pinned in `.terraform.lock.hcl`, verifying `zh:` archive hashes before starting (downloading into Writ's cache if absent).
- **Regular HCL files only:** Refuses stacks containing `.tf.json`, `.tofu.json`, or symlinked `.tf`/`.tofu` files to prevent declaration clashes.
- **Literal provider blocks:** Provider configuration blocks must contain literal values only; supply dynamic settings via provider environment variables.
- **OpenTofu naming:** Resource types use OpenTofu names (`random_uuid`), and alternate provider configs use `NAME.ALIAS` (`aws.west`).
- **Grouping:** Objects of identical shape share one `for_each` `resource` and `import` block (objects with differing nested blocks stay separate).
- **Readable names:** Resource names and `for_each` keys never contain ARNs or 12-digit AWS account IDs ([How resources are named](#how-resources-are-named)).
- **Sensitive attributes:** Secrets become `variable` blocks with `sensitive = true` (set via `TF_VAR_*`). Objects whose import ID is itself sensitive (such as `random_password`) are marked `not imported` and skipped because OpenTofu refuses sensitive import IDs.
- **Runtime fields and heredocs:** Runtime-owned attributes go into `lifecycle { ignore_changes = [...] }`; multiline strings (such as Nomad jobspecs) format as heredocs.
- **Unicode NFC required:** OpenTofu normalizes strings to NFC; non-NFC import IDs or attribute strings are marked `not imported` to avoid replacement diffs.
- **Safe output target:** `--out` must be a new `.tf` file (refused if it, its `.imports.json`, `X.tofu`, `X.tf.json`, or `X.tofu.json` exists, or if named `override.tf` / `*_override.tf`).

### How resources are named

Names and `for_each` keys come from `name`, then the `Name` tag, then the import ID. Types whose import ID is an ARN without a `name` attribute derive names from configuration (and their parent if imported in the same run):

| Type | Built from | Example |
| --- | --- | --- |
| `aws_lb_listener` | load balancer, protocol, port | `aws_lb_listener_web_http_80` |
| `aws_lb_listener_rule` | listener, then `Name` tag or priority | `aws_lb_listener_rule_web_http_80_priority_10` |
| `aws_acm_certificate` | domain name | `aws_acm_certificate_example_com` |

Labels containing `arn:` or standalone 12-digit runs (AWS account IDs) are rejected, except for the 12-hex-digit final group of a standalone 36-character UUID. Fallback names use a sequence number (`aws_acm_certificate_1`).

## Import and accept

Import five `random` objects from a selection file:

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

Excerpt from `imported.tf`:

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

Excerpt from `imported.imports.json`:

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

Save the plan and verify it with `writ import accept`:

```
$ tofu plan -out import.plan
$ writ import accept --imports imported.imports.json import.plan
Accepted: the plan imports 5 objects and changes nothing else.
```

### How writ import accept reads the plan

When `PLAN` is a saved OpenTofu plan file, `writ import accept` runs `tofu show -json PLAN` in the imports file's directory (or `--dir`), which must already be initialized. If `tofu show` fails, stderr is withheld so unmasked secrets don't leak; run `tofu show` directly to diagnose.

You can also pass a JSON plan file or `-` on stdin (a masked plan, `writ plan --remote --json`, or raw `tofu show -json`, which Writ masks first). Because JSON doesn't prove which saved plan will apply, JSON input accepts no-op imports only and refuses plans with provider-only in-place updates.

### What it accepts

A plan is accepted only when:

- The plan didn't error.
- Every object in `--imports` is imported once at its exact root-module address and import ID, as a no-op or an allow-listed provider-only update.
- No extra imports or resource changes exist (existing state resources must be `no-op`).

Any mismatch (wrong address, module import, duplicate import, wrong ID, missing object, or non-allow-listed change) is refused with the offending address.

### Provider-only settings

Some providers leave provider-local settings out of an import, so the first plan updates them in place. `writ import accept` keeps a measured allow-list:

| Provider | Version | Type | Settings |
| --- | --- | --- | --- |
| `registry.opentofu.org/hashicorp/nomad` | 2.6.1 | `nomad_job` | `deregister_on_destroy`, `deregister_on_id_change`, `detach`, `preserve_counts`, `read_allocation_ids`, `rerun_if_dead` |

For a saved plan file, Writ accepts an in-place update (no replace path) when every changed attribute is on that list for the provider address and resource type, with known, non-sensitive values. Each setting prints one line:

```
$ writ import accept --imports imported.imports.json import.plan
~ nomad_job.nomad_job_import_web_default: sets deregister_on_destroy = true, a setting that registry.opentofu.org/hashicorp/nomad 2.6.1 keeps and the cloud does not return
...
Accepted: the plan imports 1 object, sets 6 provider-only settings that the cloud does not return, and changes nothing else.
```

- `--json` lists each setting under `settings` (`address`, `attribute`, `value`, `provider`, `version`).
- Writ compares raw `tofu show -json` diffs before masking, so two different sensitive values cannot look equal, then masks everything it prints.
- After the apply, the next plan shows no changes.
- Launch templates with `update_default_version = true` are not allow-listed, because the update moves the live default version.

## Write a selection file

Pass a JSON selection file (or `-` for stdin) with an `objects` array. Each entry targets one `type` (plus optional `provider` alias like `aws.west`):

| Field | Selects |
| --- | --- |
| `ids` | Explicit import IDs. |
| `values` and `id` | One object per record in `values`, expanding template `id` (`{field}`). |
| `from` and `id` (with `over`) | One object per earlier `from` object (`{parent.ATTR}`), or per element of its `over` list (`{item.ATTR}`). |
| `list` | Objects returned by the provider's `ListResource` call ([Listing objects](#listing-objects)). |

Template rules: escape braces with `{{` / `}}`; null/missing placeholders skip that object; secret placeholders are refused; templates and built IDs cap at 2,048 bytes; total expansions cap at `10 * --max-objects`. Entries execute in order. For small sets, skip the file and pass `--type TYPE --id ID...`.

## Listing objects

`list` entries call the plugin protocol's `ListResource` RPC using the resource type's list configuration schema. Example for `aws_subnet` in `hashicorp/aws` 6.70.0:

```json
{
  "objects": [
    { "type": "aws_subnet", "list": { "filter": [{ "name": "vpc-id", "values": ["vpc-00000000000000001"] }] } }
  ]
}
```

- Unknown fields or `ValidateListResourceConfig` errors refuse the entry before listing.
- Writ extracts the single required string import attribute from each returned identity (for example `id` for `aws_subnet`, `name` for `aws_iam_role`, `bucket` for `aws_s3_bucket`), skips already-selected IDs, and reads each object through the entry's provider configuration.
- Listings respect `--max-objects` (counting only newly selected IDs), deduplicate up to `4 * needed + 16` raw items before marking `not imported`, fail the whole entry on any item error, cap warnings at 20 / 16 KiB, and cap identities at 16 KiB / IDs at 2,048 bytes.
- Secrets inside a `list` query are masked and withhold provider refusal details.
- Types without a `ListResource` implementation (such as `random`, `tls`, `local`), or with zero or multiple required identity attributes (such as `aws_ecs_service` or `aws_route53_record`), report `not imported`:

```json
{"kind":"not imported","from":"objects[0]","reason":"provider hashicorp/random does not list objects of random_uuid; select them by ids, values, or from"}
```

(In `hashicorp/aws` 6.70.0, 259 types implement `ListResource` and 179 have a single required string identity attribute.)

## Refusals and skips

| Case | Result |
| --- | --- |
| `--out` exists before or appears during the run | `usage` error; nothing written |
| Empty ID or ID > 2,048 bytes | `usage` error before reads |
| Provider finds no object or rejects ID | `not imported`; nothing written |
| Template expansions exceed `10 * --max-objects` | `usage` error; nothing written |
| Duplicate object in `ids` or `values` (by ID or resolved object) | `usage` error naming both entries; nothing written |
| `from` derives an already-selected object | `skipped` |
| Missing placeholder or empty `from` parent type | `skipped` |
| Zero objects left to import | `usage` error; nothing written |

## Limits

- `--max-objects` (default `500`): max expanded objects per run (checked before reads; excess aborts without writing).
- `--max-bytes` (default `1 MiB`): max formatted output size.
- Selection file max: `4 MiB`.
- Secret comparison index max: `64 MiB` of distinct secrets >= 8 bytes (about 5 bytes memory per byte of secret, plus a second index for PEM keys).
- Provider message max: `64 MiB` per value; up to `10` concurrent reads.

## What it doesn't do

- Doesn't import by multi-attribute resource identity (`ids`, `values`, or `from` required).
- Re-reads each listed object by ID (50 listed objects = 1 list call + 50 reads).
- `from` references only objects read in the same run, not existing stack state.
- Doesn't merge objects with differing nested block shapes into one `for_each`.
- Can't import types whose provider doesn't support import (`local_file`, `tls_*`).

## JSON

`writ import bulk --json` outputs `writ.import-bulk/v0`, and `writ import accept --json` outputs `writ.import-accept/v0` ([JSON output](json-output.md)).

## Next steps

- [Commands](commands.md): full CLI reference.
- [Plan and apply](apply.md): plan and apply generated imports on the control plane.
- [How a plan becomes an apply](plan-to-apply.md): how Writ seals and approves the resulting plan.
