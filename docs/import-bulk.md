# Import many objects

Generate `resource` and `import` blocks for existing infrastructure, then check the import plan before you apply. The rules behind each step are in [How import bulk works](how-import-bulk-works.md).

```sh
writ import bulk SELECTION.json [--out FILE] [--max-objects N] [--max-bytes N] [--json]
writ import bulk --type TYPE --id ID... [--provider ADDR] [--out FILE] [--max-objects N] [--max-bytes N] [--json]
writ import accept --imports FILE PLAN [--json]
```

## Before you begin

- A stack directory with `.terraform.lock.hcl`, `.tf` or `.tofu` files, no `.writ` file, and no `.tf.json`, `.tofu.json`, or symlinked `.tf` files.
- Provider blocks with literal values only. Pass dynamic settings, such as credentials, through provider environment variables: `writ import bulk` passes your environment to providers.
- For `writ import accept` on a saved plan: an initialized stack (`tofu init`).

## Import and accept

1. Select the objects. For a few, pass `--type TYPE --id ID...`. For more, write a selection file ([Selection files](how-import-bulk-works.md#selection-files)):

   ```json
   {
     "objects": [
       { "type": "aws_subnet", "list": { "filter": [{ "name": "vpc-id", "values": ["vpc-00000000000000001"] }] } }
     ]
   }
   ```

2. Generate the code (output below is an excerpt). It writes `imported.tf` (or `--out`) and a manifest, `imported.imports.json`, and neither plans nor writes state:

   ```
   $ writ import bulk select.json
   + random_uuid.random_uuid["378c806e-ce8f-66fe-532b-b6ff92fd129f"]                 random_uuid     "378c806e-ce8f-66fe-532b-b6ff92fd129f"
   + random_string.random_string_x4yq6cmdasfw                                        random_string   "x4yq6cMdaSFW"
   wrote imported.tf: 2 resources with for_each, 1 single resource, 2 references in place of literals, 0 typed references left as literals
   wrote imported.imports.json: the objects that tofu plan should import
   Read 5 objects and wrote no state. tofu plan should show 5 to import, 0 to add, 0 to change, 0 to destroy; save it with tofu plan -out=PLAN and check it with writ import accept --imports imported.imports.json PLAN.
   ```

   Secret attributes become `sensitive` variables: set them through `TF_VAR_*`.

3. Save a plan and check it:

   ```
   $ tofu plan -out import.plan
   $ writ import accept --imports imported.imports.json import.plan
   Accepted: the plan imports 5 objects and changes nothing else.
   ```

   `writ import accept` refuses a plan that errored, imports anything else, misses an object, or changes anything else, and names the offending address ([What writ import accept accepts](how-import-bulk-works.md#what-writ-import-accept-accepts)).

4. Apply the plan. The next plan shows no changes.

## Troubleshooting

| Problem | Fix |
| --- | --- |
| `. is not an OpenTofu stack` | Run it in a stack directory with `.tf` files and a `.terraform.lock.hcl`, after `tofu init`. |
| `--out` refused | `--out` must name a new `.tf` file, not `override.tf` or `*_override.tf`, and none of its sibling files may exist. |
| `not imported` for an object | The provider found no object, the ID is sensitive or not NFC, or the type cannot be listed. Nothing is written; see [Refusals and skips](how-import-bulk-works.md#refusals-and-skips). |
| `usage` past `--max-objects` | Raise `--max-objects` (default `500`) or narrow the selection. |
| `writ import accept` refuses an in-place update | Only allow-listed provider-only settings pass, and only on a saved plan ([Provider-only settings](how-import-bulk-works.md#provider-only-settings)). |
| `tofu show` failed under `writ import accept` | Its stderr is withheld so secrets cannot leak. Run `tofu show` yourself to see why. |

## JSON

`writ import bulk --json` outputs `writ.import-bulk/v0`, and `writ import accept --json` outputs `writ.import-accept/v0` ([JSON output](json-output.md)).

## Next steps

- [How import bulk works](how-import-bulk-works.md): naming, accept rules, selection files, listing, and limits.
- [Plan and apply](apply.md): plan and apply generated imports on the control plane.
- [Commands](commands.md): full CLI reference.
