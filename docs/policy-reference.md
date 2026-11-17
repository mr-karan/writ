# Policy input and built-ins

A policy reads one input, the masked plan document, and can call a fixed list of Rego built-ins. Everything it does runs in a bounded child process. [Policy](policy.md) explains when policies run, and [Write and test a policy](write-a-policy.md) shows how to write one.

## Input

The input is the masked plan document as `writ serve` stores it, with one key added, `input.writ`. It has `format_version`, `terraform_version`, `timestamp`, `errored`, `resource_changes`, `resource_drift`, `output_changes`, `checks`, and `relevant_attributes` as `tofu show -json` writes them, with these differences, which [the mask](secrets.md) makes:

- A value under a sensitive mark is the string `(sensitive)`.
- `planned_values` and `prior_state` are dropped. They repeat the changes.
- `variables` is a list of the variables' names, with no values.
- `configuration` has `providers`, each with `key`, `name`, `full_name`, `alias`, `version_constraint`, and `module_address`, and `modules`, each with `address`, `source`, and `version_constraint`.

A schema of the input type checks every rule at start. A rule that reads a key the input never has, such as `input.planned_values`, is refused, because under `writ serve` it would never deny.

`input.writ`:

| Key | At the plan point | At the apply point |
| --- | --- | --- |
| `decision` | `"plan"` | `"apply"` |
| `now` | When the plan was evaluated, RFC 3339, UTC | When the apply was asked for |
| `environment` | The plan's environment | The same |
| `stack` | The plan's stack | The same |
| `actor` | The plan's author: `person`, `display`, `kind`, and `teams` | The person or CI job that asks for the apply |
| `approvals` | `[]` | The approvals that count: `person`, `display`, and `via` |

Under `conftest`, `input.writ` is absent. A rule that reads it gets no value there, and gives no message. To read it with a default, use `object.get`, as `examples/policy/writ.rego` does:

```rego
environment := object.get(input, ["writ", "environment"], "")
```

## Built-ins

A policy can call only the built-ins below. A rule that calls another fails to compile, so `writ serve` doesn't start. `internal/policy/builtins.go` has the same list, and a test keeps the two equal.

| Family | Built-ins | Why |
| --- | --- | --- |
| Operators | `eq`, `assign`, `equal`, `neq`, `gt`, `gte`, `lt`, `lte`, `plus`, `minus`, `mul`, `div`, `rem`, `and`, `or`, `internal.member_2`, `internal.member_3` | Comparison, arithmetic, set intersection and union, and `in`. Every rule needs them |
| Numbers | `abs`, `round`, `ceil`, `floor`, `to_number`, `format_int` | Counts and limits |
| Aggregates | `count`, `sum`, `max`, `min`, `sort` | A rule over the changes of a plan |
| Collections | `array.concat`, `array.slice`, `array.reverse`, `intersection`, `union`, `object.get`, `object.keys`, `object.union` | Reading the input's lists and objects, and `input.writ` where conftest has none |
| Types | `is_string`, `is_number`, `is_boolean`, `is_array`, `is_set`, `is_object`, `is_null`, `type_name` | A value of a change can be any type, or `null` |
| Strings | `concat`, `contains`, `startswith`, `endswith`, `lower`, `upper`, `split`, `replace`, `trim`, `trim_space`, `trim_prefix`, `trim_suffix`, `indexof`, `substring`, `sprintf` | Addresses, types, and messages |
| Patterns | `regex.match`, `glob.match` | Matching addresses and types. Go's `regexp` runs in linear time |
| Time | `time.parse_rfc3339_ns`, `time.parse_ns`, `time.parse_duration_ns`, `time.date`, `time.clock`, `time.weekday`, `time.add_date`, `time.diff`, `time.format` | Change windows over `input.writ.now`. `"Local"` is UTC |
| Encoding | `json.marshal` | A message that shows a value of the input |

Writ leaves out every other built-in of OPA v1.22.4, for one of these reasons:

- `http.send`, `net.lookup_ip_addr`, `providers.aws.sign_req`, and `opa.runtime` read the network, the host, or its environment.
- `time.now_ns`, `rand.intn`, and `uuid.rfc4122` read the clock or randomness, so a decision wouldn't depend on its input alone. A rule reads the time from `input.writ.now`.
- `json.verify_schema` and `json.match_schema` follow a `file://` reference. A security review read a host file into a message with one.
- `strings.render_template`, every `bits.*`, `numbers.range`, and `numbers.range_step` can run past a deadline or allocate gigabytes in one call.
- `print` and `trace` write the input out.
- `walk`, `yaml.*`, the encodings, `graphql.*`, `rego.*`, and the rest have no use here yet.

## Bounds and failures

`writ serve` evaluates every set of one decision in a child process: the `writ` binary itself, with UTC as its only time zone, no environment, and only stdin and stdout. It closes every descriptor that it inherited. Its garbage collector works hard from half of `memory`, and on Linux its address space can grow by `memory` past what it maps at its start. `writ serve` kills it at `timeout`. The input it gets is at most 16 MiB.

A decision keeps at most 100 messages of each set. The mask sweeps each message whole, and then cuts it at 1,000 bytes. A cut that falls inside a `(sensitive)` marker cuts before the marker.

Every failure that Writ detects denies at a mandatory set's level, so a plan that a failed policy checked can't be approved:

- The process ran past `timeout`, or past its memory, or crashed, or wrote anything but the protocol's lines. Every set is an error.
- A rule's built-in failed, such as `to_number("seven")`. Built-in errors are strict.
- A built-in panicked.
- A rule's value isn't a set of messages, or a message isn't a string.
- The input couldn't be built, or is larger than 16 MiB.

**Limits.** A condition that's false for a wrong reason isn't detected. `input.writ.environment == "production"` where the environment is `prod` gives no value, which reads as no deny. A set's tests guard against it.
