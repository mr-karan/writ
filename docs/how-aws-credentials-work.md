# How AWS credentials work

How `writ serve` hands short-lived AWS credentials to each run, what CloudTrail records, and the rules for S3 state. To set up roles, see [AWS roles and credentials](aws-credentials.md).

## How credentials reach providers and CloudTrail

1. `writ serve` runs a loopback AWS container credentials endpoint.
2. Each run gets `AWS_CONTAINER_CREDENTIALS_FULL_URI`, a random per-run `AWS_CONTAINER_AUTHORIZATION_TOKEN`, and `AWS_EC2_METADATA_DISABLED=true`.
3. Long runs refresh credentials 15 minutes before expiry.
4. When the run finishes, its bearer token is revoked. The run's [mask](secrets.md) tracks every issued `SecretAccessKey` and `SessionToken` (including refreshes) and scrubs them from stored outputs and logs.

- An env with no `plan_role` gives its providers no AWS credential.
- A provider's environment holds only the credential endpoint and its token, the `provider_env` values, `PATH`, `AWS_EC2_METADATA_DISABLED=true`, `HOME` and `TMPDIR` set to the provider's own directory, and the plugin settings.
- Providers do not inherit the environment of `writ serve`. A variable such as `AWS_REGION`, or a proxy setting in its unit, does not reach them: set it in `provider_env`.

CloudTrail session attribution:

| Field | Value |
| --- | --- |
| `RoleSessionName` | `writ-WHO-ID` (`ci`, person display name, or `drift` + plan ref, run ID, `adopt-...`, or `index-...`; truncated to 64 chars keeping `ID`) |
| `SourceIdentity` | `gitlab-PROJECTID-PIPELINEID`, `github-REPOID-RUNID`, person ID (`gitlab-user-2002`), `writ-serve-drift`, or `writ-serve-index` |
| `DurationSeconds` | `aws.session` (default `1h`) |

Each STS assumption logs the session name, source identity, and access key ID:

```
{"time":"2026-11-10T19:21:56.87338551+05:30","level":"INFO","msg":"assumed role","component":"credentials","role":"arn:aws:iam::222222222222:role/writ-plan","session":"writ-ci-OH7MRPZTDKWIQBRXFHJDKV3GAD","source_identity":"gitlab-4242-1849213","why":"start","access_key_id":"ASIATEST000000000001","expires":"2026-11-10T14:51:56Z"}
```

Output of a real run against a local STS stand-in, 2026-11-10.

## S3 backend rules

- Literal values only (no expressions or variables), with `use_lockfile = true` or a non-empty `dynamodb_table`.
- Only allow-listed OpenTofu 1.13.9 attributes: `bucket`, `key`, `region`, `workspace_key_prefix`, `use_lockfile`, `dynamodb_table`, `encrypt`, `kms_key_id`, `use_path_style`, `force_path_style`, `use_dualstack_endpoint`, `use_fips_endpoint`, `skip_credentials_validation`, `skip_region_validation`, `skip_requesting_account_id`, `skip_s3_checksum`, `max_retries`, `retry_mode`, and `endpoints`. Inline credentials, roles, profiles, ACLs, proxies, and TLS overrides are refused.
- Custom `endpoints = { s3 = "..." }` must match `env.s3_endpoints` exactly.
- Requires `plan_role` and `apply_role` (not supported under `writ serve --dev`). OpenTofu state and plan encryption aren't supported because Writ reads serials from the plan and state.

## S3 serials and outside writes

Writ tracks up to 20 Writ-applied serials and 20 external serials per `stack@env`. Any S3 serial not written by a Writ apply is flagged as `outside_write` in the UI, `GET /v1/drift`, and `GET /v1/index`, logged at `WARN` (`state written outside writ`), and sent to matching `outside-write` [notifiers](notifications.md) once per serial. Writ refuses to index or approve plans against that serial until you run [`writ state adopt`](adopt-state.md) or apply a newer Writ plan.

## Limits

- Built-in role assumption supports AWS STS only; other providers use `provider_env` files.
- Revoking the loopback token stops future refreshes; STS credentials already fetched remain valid until `aws.session` expires (though scrubbed from all Writ outputs).
- Between `tofu apply` releasing an S3 state lock and Writ reading the resulting serial, an external same-lineage write could be attributed to that apply.
- S3 state and STS flows have been tested against local stand-ins, not a live AWS account.
