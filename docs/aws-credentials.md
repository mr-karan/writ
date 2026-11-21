# AWS roles and credentials

Configure `writ serve` to assume separate read-only plan roles and write-capable apply roles via AWS STS, serving short-lived credentials to each OpenTofu run over loopback. CI jobs and laptops hold no AWS credentials, and every CloudTrail event carries the requesting person or CI pipeline in `SourceIdentity`.

## Prerequisites

- A Linux host running a team's `writ serve` with its own AWS identity (instance profile, web identity, shared credentials, or standard AWS SDK env vars).
- `provider_uids` configured (`writ serve --check` refuses AWS roles without `provider_uids`).
- The [`nftables` metadata firewall rule](run-isolation.md) active so provider UIDs can't reach `169.254.169.254` or `fd00:ec2::254`.

## Steps

### 1. Create plan and apply IAM roles in AWS

Create two IAM roles per environment:

- **`plan_role`:** Read-only infrastructure permissions (enforcing read-only scope in IAM is your responsibility).
- **`apply_role`:** Write permissions for that environment; assumed only for approved or overridden applies.

Trust `writ serve`'s AWS identity for both `sts:AssumeRole` and `sts:SetSourceIdentity`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "AWS": "arn:aws:iam::000000000000:role/writ-control-plane"
      },
      "Action": [
        "sts:AssumeRole",
        "sts:SetSourceIdentity"
      ]
    }
  ]
}
```

### 2. Grant S3 state permissions (if using `backend "s3"`)

For stacks storing state in S3 instead of Writ's HTTP backend:

| Role | Required permissions | Must not have |
| --- | --- | --- |
| `plan_role` | `s3:ListBucket`, `s3:GetObject` on the state key | `s3:PutObject`, `s3:DeleteObject` on the state key and `KEY.tflock` |
| `apply_role` | `s3:ListBucket`, `s3:GetObject`, `s3:PutObject` on the state key and `KEY.tflock`, `s3:DeleteObject` on `KEY.tflock` | |

`backend "s3"` blocks must use literal attributes only and set `use_lockfile = true` or a non-empty `dynamodb_table`. List custom S3 endpoint URLs in the environment's `s3_endpoints`.

> **Security boundary for S3 state:** Writ grants don't stop one stack's `backend "s3"` from naming another stack's key in the same environment. Isolate mutually untrusted S3 stacks into separate environments with key-prefix-scoped IAM roles.

### 3. Configure `aws` and `env` in your server config

```hcl
provider_uids = "61000-61063"

aws {
  region  = "eu-west-1"
  session = "1h" # 15m to 12h, default 1h
  # sts_endpoint = "https://sts.eu-west-1.amazonaws.com" # optional VPC interface endpoint
}

env "staging" {
  key        = "/var/lib/writ/keys/staging.key"
  plan_role  = "arn:aws:iam::000000000000:role/writ-staging-plan"
  apply_role = "arn:aws:iam::000000000000:role/writ-staging-apply"
}

env "prod" {
  key          = "/var/lib/writ/keys/prod.key"
  plan_role    = "arn:aws:iam::000000000000:role/writ-prod-plan"
  apply_role   = "arn:aws:iam::000000000000:role/writ-prod-apply"
  provider_env = { NOMAD_TOKEN = "/run/secrets/prod/nomad_token" }
}
```

Use `provider_env` (variable name to secret file path) for non-AWS providers or `TF_VAR_*` inputs. Keys prefixed with `AWS_`, `TF_` (other than `TF_VAR_`), `PLUGIN_`, `WRIT_`, `HOME`, or `TMPDIR` are refused.

### 4. Check the configuration

```sh
writ serve --config /etc/writ/serve --check
```

### What you should see

```
aws: sts in eu-west-1 at the regional endpoint, sessions of 1h
role: prod plan arn:aws:iam::222222222222:role/writ-plan
role: staging plan arn:aws:iam::111111111111:role/writ-plan
isolation: uid 61000 cannot read provider_env NOMAD_TOKEN of env prod, /var/lib/writ/secrets/nomad_token: ok
```

Output of a real run, 2026-11-16.

Each STS assumption logs session name, source identity, and access key ID:

```
{"time":"2026-11-10T19:21:56.87338551+05:30","level":"INFO","msg":"assumed role","component":"credentials","role":"arn:aws:iam::222222222222:role/writ-plan","session":"writ-ci-OH7MRPZTDKWIQBRXFHJDKV3GAD","source_identity":"gitlab-4242-1849213","why":"start","access_key_id":"ASIATEST000000000001","expires":"2026-11-10T14:51:56Z"}
```

Output of `just test-serve`, 2026-11-10.

## How credentials reach providers and CloudTrail

1. `writ serve` runs a loopback AWS container credentials endpoint.
2. Each run gets `AWS_CONTAINER_CREDENTIALS_FULL_URI`, a random per-run `AWS_CONTAINER_AUTHORIZATION_TOKEN`, and `AWS_EC2_METADATA_DISABLED=true`.
3. Long runs refresh credentials 15 minutes before expiry.
4. When the run finishes, its bearer token is revoked. The run's [mask](secrets.md) tracks every issued `SecretAccessKey` and `SessionToken` (including refreshes) and scrubs them from stored outputs and logs.

CloudTrail session attribution:

| Field | Value |
| --- | --- |
| `RoleSessionName` | `writ-WHO-ID` (`ci`, person display name, or `drift` + plan ref, run ID, `adopt-...`, or `index-...`; truncated to 64 chars keeping `ID`) |
| `SourceIdentity` | `gitlab-PROJECTID-PIPELINEID`, `github-REPOID-RUNID`, person ID (`gitlab-user-2002`), `writ-serve-drift`, or `writ-serve-index` |
| `DurationSeconds` | `aws.session` (default `1h`) |

## Troubleshooting

- **Immediate `denied` naming the role**: Confirm the trust policy allows both `sts:AssumeRole` and `sts:SetSourceIdentity` and that the role's max session duration >= `aws.session`.
- **`--check` refuses a `provider_env` key**: Remove reserved prefixes (`AWS_`, non-`TF_VAR_` `TF_`, `PLUGIN_`, `WRIT_`, `HOME`, `TMPDIR`).
- **S3 stack refused**: Use literal values only, set `use_lockfile = true` or `dynamodb_table`, remove inline credentials/profiles/roles, and match any custom `endpoints.s3` to `env.s3_endpoints`.
- **S3 stack fails under `--dev`**: Dev mode has no credential broker; use `backend "http" {}`.

## Limits

- Built-in role assumption supports AWS STS only; other providers use `provider_env` files.
- Revoking the loopback token stops future refreshes; STS credentials already fetched remain valid until `aws.session` expires (though scrubbed from all Writ outputs).
- S3 state and STS flows have been tested against local stand-ins (`versitygw`, STS stand-in, AWS Emulator), not a live AWS account.

## Next steps

- [Run isolation and egress](run-isolation.md): provider UIDs, namespaces, cgroups, and metadata firewalling.
- [Secrets and the mask](secrets.md): how assumed credentials and `provider_env` stay masked.
- [Config](config.md): full `aws` and `env` reference.
