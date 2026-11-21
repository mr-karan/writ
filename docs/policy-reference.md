# Policy input and built-ins

Policies evaluate one input (the masked plan document plus `input.writ`) using a fixed allow-list of Rego built-ins inside a bounded child process. See [Write and test policies](write-a-policy.md) and the [Policy cookbook](policy-cookbook.md).

## Input

`input` is the masked plan document stored by `writ serve`, matching `tofu show -json` keys (`format_version`, `terraform_version`, `timestamp`, `errored`, `resource_changes`, `resource_drift`, `output_changes`, `checks`, `relevant_attributes`) with four differences from [the mask](secrets.md):

- Sensitive values become `"(sensitive)"`.
- `planned_values` and `prior_state` are omitted.
- `variables` lists variable names only, with no values.
- `configuration` includes only `providers` (`key`, `name`, `full_name`, `alias`, `version_constraint`, `module_address`) and `modules` (`address`, `source`, `version_constraint`).

Writ type-checks every policy against this schema at startup. Referencing an absent key such as `input.planned_values` fails startup so broken rules can't silently pass.

`input.writ`:

| Key | At the plan point | At the apply point |
| --- | --- | --- |
| `decision` | `"plan"` | `"apply"` |
| `now` | Evaluation timestamp (RFC 3339, UTC) | Apply request timestamp (RFC 3339, UTC) |
| `environment` | Plan environment | Same |
| `stack` | Plan stack | Same |
| `actor` | Plan author (`person`, `display`, `kind`, `teams`) | Caller requesting apply |
| `approvals` | `[]` | Counting approvals (`person`, `display`, `via`) |

Under `conftest`, `input.writ` is absent. Use `object.get` for defaults, as in `examples/policy/writ.rego`:

```rego
environment := object.get(input, ["writ", "environment"], "")
```

## Built-ins

Policies may call only the built-ins below. Calling any other built-in fails compilation at startup.

| Family | Built-ins | Purpose |
| --- | --- | --- |
| Operators | `eq`, `assign`, `equal`, `neq`, `gt`, `gte`, `lt`, `lte`, `plus`, `minus`, `mul`, `div`, `rem`, `and`, `or`, `internal.member_2`, `internal.member_3` | Comparisons, arithmetic, set operations, and `in` |
| Numbers | `abs`, `round`, `ceil`, `floor`, `to_number`, `format_int` | Counts and limits |
| Aggregates | `count`, `sum`, `max`, `min`, `sort` | Summarizing plan changes |
| Collections | `array.concat`, `array.slice`, `array.reverse`, `intersection`, `union`, `object.get`, `object.keys`, `object.union` | Reading lists, objects, and optional `input.writ` fields |
| Types | `is_string`, `is_number`, `is_boolean`, `is_array`, `is_set`, `is_object`, `is_null`, `type_name` | Type guards on dynamic attribute values |
| Strings | `concat`, `contains`, `startswith`, `endswith`, `lower`, `upper`, `split`, `replace`, `trim`, `trim_space`, `trim_prefix`, `trim_suffix`, `indexof`, `substring`, `sprintf` | Addresses, resource types, and messages |
| Patterns | `regex.match`, `glob.match` | Linear-time Go `regexp` and glob matching |
| Time | `time.parse_rfc3339_ns`, `time.parse_ns`, `time.parse_duration_ns`, `time.date`, `time.clock`, `time.weekday`, `time.add_date`, `time.diff`, `time.format` | Change windows over `input.writ.now` (`"Local"` is UTC) |
| Encoding | `json.marshal` | Formatting input values in messages |

All other OPA v1.22.4 built-ins are excluded:

- `http.send`, `net.lookup_ip_addr`, `providers.aws.sign_req`, and `opa.runtime`: access network, host, or environment.
- `time.now_ns`, `rand.intn`, and `uuid.rfc4122`: non-deterministic (use `input.writ.now`).
- `json.verify_schema` and `json.match_schema`: can resolve `file://` references on disk.
- `strings.render_template`, `bits.*`, `numbers.range`, and `numbers.range_step`: unbounded CPU or memory allocation.
- `print` and `trace`: leak raw input.
- `walk`, `yaml.*`, `graphql.*`, `rego.*`, and remaining built-ins: unused here.

## Bounds and failures

`writ serve` evaluates every set for a decision in an isolated child process with `TZ=UTC`, empty environment, closed inherited file descriptors, and stdin/stdout only. Memory is soft-limited from half of `memory`, and on Linux address space above the startup mapping is capped at `memory`; execution is killed at `timeout`, and input is capped at 16 MiB.

Each set keeps up to 100 messages. Messages are masked whole and truncated to 1,000 bytes (cutting before any `(sensitive)` marker that would be split).

Evaluation failures fail closed (denying for mandatory sets):

- Process timeout, OOM, crash, or invalid protocol output (errors all sets).
- Strict built-in error (such as `to_number("seven")`) or panic.
- Rule returns a non-set or non-string message.
- Input construction failure or size over 16 MiB.

**Limits:** Logic bugs that evaluate cleanly to false (for example `input.writ.environment == "production"` when the environment is `"prod"`) can't be detected automatically. Cover them with `conftest verify` unit tests.
