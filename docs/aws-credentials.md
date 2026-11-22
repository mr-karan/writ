# AWS roles and credentials

Have `writ serve` assume a read-only plan role and a write-capable apply role through AWS STS, and serve short-lived credentials to each OpenTofu run over loopback. CI jobs and laptops hold no AWS credentials, and every CloudTrail event names the requesting person or CI pipeline in `SourceIdentity`.

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

`backend "s3"` must use literal values, `use_lockfile = true` or a `dynamodb_table`, and only allow-listed attributes; inline credentials, roles, and profiles are refused, and it does not work under `writ serve --dev` ([S3 backend rules](how-aws-credentials-work.md#s3-backend-rules)).

> **Security boundary for S3 state:** Writ grants don't stop one stack's `backend "s3"` from naming another stack's key in the same environment. Isolate mutually untrusted S3 stacks into separate environments with key-prefix-scoped IAM roles.

A serial Writ did not write is flagged as an outside write: Writ refuses to index that serial and blocks approvals until you run [`writ state adopt`](adopt-state.md) or apply a newer Writ plan ([S3 serials and outside writes](how-aws-credentials-work.md#s3-serials-and-outside-writes)).

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

Use `provider_env` (variable name to secret file path) for non-AWS providers or `TF_VAR_*` inputs.

> **Warning:** `provider_env` refuses names that start with `AWS_`, `TF_` (other than `TF_VAR_`), `PLUGIN_`, or `WRIT_`, and the names `HOME` and `TMPDIR`.

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

## How credentials reach providers

Each run gets credentials from a loopback endpoint, refreshed before expiry and revoked when the run ends. Session names and `SourceIdentity` values are in [How AWS credentials work](how-aws-credentials-work.md#how-credentials-reach-providers-and-cloudtrail).

> **Warning:** providers do not inherit the environment of `writ serve`. A variable such as `AWS_REGION`, or a proxy setting in its unit, does not reach them: set it in `provider_env`. An env with no `plan_role` gives its providers no AWS credential.

## Troubleshooting

| Problem | Fix |
| --- | --- |
| Immediate `denied` naming the role | The trust policy must allow `sts:AssumeRole` and `sts:SetSourceIdentity`, and the role's maximum session duration must be at least `aws.session`. |
| `--check` refuses a `provider_env` key | Rename it. Names starting with `AWS_`, `TF_` (other than `TF_VAR_`), `PLUGIN_`, or `WRIT_` are reserved, and so are `HOME` and `TMPDIR`. |
| S3 stack refused | Use literal values only, set `use_lockfile = true` or `dynamodb_table`, remove inline credentials, profiles, and roles, and match any custom `endpoints.s3` to `env.s3_endpoints`. |
| S3 stack fails under `--dev` | Dev mode has no credential broker. Use `backend "http" {}`. |

Limits of the credential broker and S3 tracking are in [Limits](how-aws-credentials-work.md#limits).

## Next steps

- [Run isolation and egress](run-isolation.md): provider UIDs, namespaces, cgroups, and metadata firewalling.
- [Secrets and the mask](secrets.md): how assumed credentials and `provider_env` stay masked.
- [Config](config.md): full `aws` and `env` reference.
