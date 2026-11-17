# A whole writ serve config, as docs/config.md shows it.
url           = "https://writ.example.com"
listen        = "0.0.0.0:443"
database      = "postgres://writ@/writ?host=/run/postgresql"
data          = "/var/lib/writ"
provider_uids = "61000-61063"
plan_ttl      = "24h"

tls {
  cert = "/var/lib/writ/tls/fullchain.pem"
  key  = "/var/lib/writ/tls/key.pem"
}

aws {
  region  = "eu-west-1"
  session = "1h"
}

limits {
  requests_per_second  = 10
  requests_per_address = 20
  plans_per_principal  = 4
  plans                = 8
  applies              = 4
  body                 = "16MiB"
  plan_timeout         = "30m"
  apply_start_timeout  = "10m"
  apply_timeout        = "4h"
}

retention {
  audit = "400d"
  plans = "30d"
}

issuer "gitlab" {
  kind     = "gitlab-ci"
  identity = "gitlab"
  url      = "https://gitlab.com"
}

team "infra_mr" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], pipeline_source = ["merge_request_event"] }
}

team "infra_reviewers" {
  issuer = "gitlab"
  match  = { project_id = ["4242"], user_access_level = ["maintainer", "owner"] }
}

team "infra_main" {
  issuer = "gitlab"
  match = {
    project_id        = ["4242"]
    ref_path          = ["refs/heads/main"]
    ref_protected     = ["true"]
    ci_config_ref_uri = ["gitlab.com/acme/infra//.gitlab-ci.yml@refs/heads/main"]
  }
}

grant {
  teams  = ["infra_mr"]
  stacks = ["*"]
  envs   = ["staging", "prod"]
  can    = ["read", "plan"]
  risks  = []
}

grant {
  teams  = ["infra_reviewers"]
  stacks = ["*"]
  envs   = ["prod"]
  can    = ["approve"]
}

grant {
  teams  = ["infra_main"]
  stacks = ["*"]
  envs   = ["staging", "prod"]
  can    = ["read", "apply"]
  risks  = ["create", "update", "replace", "delete"]
}

approval {
  stacks = ["*"]
  envs   = ["prod"]
  risks  = ["create", "update", "replace", "delete", "data-loss"]
  need   = 1
}

approval {
  stacks = ["*"]
  envs   = ["*"]
  risks  = ["delete", "data-loss"]
  need   = 1
}

env "staging" {
  key          = "/var/lib/writ/keys/staging.key"
  plan_role    = "arn:aws:iam::111111111111:role/writ-plan"
  apply_role   = "arn:aws:iam::111111111111:role/writ-apply"
  s3_endpoints = ["https://s3.storage.example.com", "http://127.0.0.1:7070"]
}

env "prod" {
  key          = "/var/lib/writ/keys/prod.key"
  plan_role    = "arn:aws:iam::222222222222:role/writ-plan"
  apply_role   = "arn:aws:iam::222222222222:role/writ-apply"
  provider_env = { NOMAD_TOKEN = "/run/secrets/prod/nomad_token" }
}

