# AWS roles and credentials

Configure `writ serve` to assume separate read-only plan roles and write-capable apply roles through AWS STS, and serve short-lived per-run credentials to providers over loopback.

With this setup, CI jobs and laptops hold no AWS credentials. Only `writ serve` calls `sts:AssumeRole`, and every call is tagged in CloudTrail with the person or CI pipeline that requested it.

## Prerequisites

- A Linux host running a team's `writ serve` with its own AWS identity (instance profile, web identity, shared credentials, or environment variables found by the AWS SDK's default chain).
- `provider_uids` configured on the server. `writ serve --check` refuses any config that sets an AWS role without `provider_uids`.
- The [`nftables` metadata firewall rule](run-isolation.md) active on the host so providers cannot reach `169.254.169.254` or `fd00:ec2::254`.

## Steps

### 1. Create plan and apply IAM roles in AWS

Create two IAM roles per environment:

- **Plan role** (for example `arn:aws:iam::000000000000:role/writ-plan`): Grant read-only permissions for your infrastructure. Writ assumes whatever role you configure, so keeping `plan_role` read-only is your responsibility.
- **Apply role** (for example `arn:aws:iam::000000000000:role/writ-apply`): Grant the permissions required to create, update, and destroy resources in that environment. Writ assumes `apply_role` only for an approved plan, or one overridden by a person with `override` and a recorded reason.

In both roles' trust policies, allow `writ serve`'s own AWS identity to call both `sts:AssumeRole` and `sts:SetSourceIdentity`:

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

If a stack keeps state in its own S3 bucket instead of Writ's built-in `http` backend, add these bucket permissions to each role:

| Role | Required permissions | Must not have |
| --- | --- | --- |
| `plan_role` | `s3:ListBucket` on the bucket, `s3:GetObject` on the state key | `s3:PutObject` and `s3:DeleteObject` on the state key and `KEY.tflock` |
| `apply_role` | `s3:ListBucket`, `s3:GetObject`, `s3:PutObject` on the state key and `KEY.tflock`, and `s3:DeleteObject` on `KEY.tflock` | |

Your stack's `backend "s3"` block must set `use_lockfile = true` or a non-empty `dynamodb_table`, and all values must be literals. If you use a custom S3 endpoint, list its exact URL in `s3_endpoints` on the environment block.

> **Security boundary for S3 state:** Writ's grants do not prevent one stack's `backend "s3"` block from pointing to another stack's key in the same environment. If two stacks must not read each other's state, put them in separate environments with IAM roles scoped to each stack's S3 key prefix.

### 3. Configure `aws` and `env` in your server config

Add the `aws` block and role ARNs to `/etc/writ/serve`:

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

For non-AWS providers or OpenTofu input variables, pass secrets via `provider_env` (a map of environment variable names to file paths on the server). Writ refuses names starting with `AWS_`, `TF_`, `PLUGIN_`, or `WRIT_`, as well as `HOME` and `TMPDIR`, except `TF_VAR_` names for OpenTofu variables.

### 4. Check the configuration

Run `writ serve --check` to verify the roles, AWS config, and provider UID isolation:

```sh
writ serve --config /etc/writ/serve --check
```

### What you should see

`writ serve --check` prints the STS region, session duration, and configured roles, and confirms that provider UIDs cannot read your `provider_env` files or reach the instance metadata endpoints:

```
aws: sts in eu-west-1 at the regional endpoint, sessions of 1h
role: prod plan arn:aws:iam::222222222222:role/writ-plan
role: staging plan arn:aws:iam::111111111111:role/writ-plan
isolation: uid 61000 cannot read provider_env NOMAD_TOKEN of env prod, /var/lib/writ/secrets/nomad_token: ok
```

Output of a real run, 2026-11-16.

When a run assumes a role, `writ serve` logs the session name, source identity, and access key ID:

```
{"time":"2026-11-10T19:21:56.87338551+05:30","level":"INFO","msg":"assumed role","component":"credentials","role":"arn:aws:iam::222222222222:role/writ-plan","session":"writ-ci-OH7MRPZTDKWIQBRXFHJDKV3GAD","source_identity":"gitlab-4242-1849213","why":"start","access_key_id":"ASIATEST000000000001","expires":"2026-11-10T14:51:56Z"}
```

Output of `just test-serve`, 2026-11-10.

## How credentials reach providers and CloudTrail

`writ serve` does not place AWS keys into the provider's environment variables. Instead:

1. `writ serve` starts a loopback credential endpoint implementing the AWS SDK container credentials protocol.
2. Each run receives `AWS_CONTAINER_CREDENTIALS_FULL_URI` and a per-run random `AWS_CONTAINER_AUTHORIZATION_TOKEN`, plus `AWS_EC2_METADATA_DISABLED=true`.
3. For long applies, the endpoint refreshes credentials 15 minutes before expiry.
4. When the run ends, the token stops working. The run's [mask](secrets.md) tracks every secret access key and session token issued during the run (including refreshes) and sweeps them from every plan, log, and diagnostic.

Every `sts:AssumeRole` request tags the session so you can trace it in CloudTrail:

| Field | Value |
| --- | --- |
| `RoleSessionName` | `writ-WHO-ID` (`ci`, person display name, or `drift`, followed by plan ref, run id, `adopt-...`, or `index-...`; truncated to AWS's 64-character limit while keeping `ID`) |
| `SourceIdentity` | `gitlab-PROJECTID-PIPELINEID`, `github-REPOID-RUNID`, person id (such as `gitlab-user-2002`), `writ-serve-drift`, or `writ-serve-index` |
| `DurationSeconds` | `aws.session` (`1h` by default) |

## Troubleshooting

- **Plan fails immediately with a `denied` diagnostic naming the role**: Verify the role's IAM trust policy allows both `sts:AssumeRole` and `sts:SetSourceIdentity` for `writ serve`'s AWS identity, and that the role's max session duration is at least `aws.session`.
- **`writ serve --check` refuses a `provider_env` key**: Remove keys prefixed with `AWS_`, `TF_` (other than `TF_VAR_`), `PLUGIN_`, `WRIT_`, `HOME`, or `TMPDIR`.
- **S3 stack refused at plan time**: Ensure the `backend "s3"` block uses literal values only, includes `use_lockfile = true` or `dynamodb_table`, has no credential/profile/role attributes, and uses `endpoints = { s3 = "..." }` matching `env.s3_endpoints` if you use a custom endpoint.
- **S3 stack fails under `writ serve --dev`**: Dev mode has no credential broker and does not run S3 stacks. Use `backend "http" {}` under `--dev`.

## Limits

- Built-in role assumption supports AWS STS roles only. Other clouds and providers use `provider_env` secret files on the server.
- Ending the loopback credential token does not revoke STS credentials a provider already fetched; the mask sweeps them from stored output, and they expire at `aws.session`.
- S3 state and AWS STS assumption have been tested against local stand-ins (`versitygw`, STS stand-in, and AWS Emulator), not a live AWS account.

## Next steps

- [Run isolation and egress](run-isolation.md): lock down provider UIDs, namespaces, and the EC2 metadata firewall rule.
- [Handle secrets and masking](secrets.md): how assumed credentials and `provider_env` values stay out of plans and logs.
- [Server config](config.md): full reference for `aws` and `env` blocks.
