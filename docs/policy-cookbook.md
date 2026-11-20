# Policy cookbook

Every recipe on this page is a working Rego rule from `examples/policy/` with its unit test from `examples/policy/policy_test.rego` or `examples/policy/cookbook_test.rego`. You can run the full test suite locally with:

```
conftest verify --policy examples/policy
```

To learn how to wire a policy directory into `writ serve`, see [Write and test a policy](write-a-policy.md). For the full input schema and allowed built-ins, see [Policy input and built-ins](policy-reference.md).

## Shared helpers (`writ.rego`)

Rules that inspect the environment, decision point, or created/updated resources use these shared definitions in `package main`:

```rego
package main

import rego.v1

# input.writ is added by writ serve. Under conftest it is absent, so these
# helpers default to "".
environment := object.get(input, ["writ", "environment"], "")

decision := object.get(input, ["writ", "decision"], "")

created_or_updated contains rc if {
	some rc in input.resource_changes
	some action in rc.change.actions
	action in {"create", "update"}
}
```

## 1. Require minimum RSA key size (`tls.rego`)

**Goal:** Block creation of RSA private keys shorter than 4096 bits.

**Rego:**

```rego
package main

import rego.v1

deny_rsa contains msg if {
	some rc in input.resource_changes
	rc.type == "tls_private_key"
	"create" in rc.change.actions
	rc.change.after.algorithm == "RSA"
	rc.change.after.rsa_bits < 4096
	msg := sprintf("%s: an RSA key needs 4096 bits, and this one has %d", [rc.address, rc.change.after.rsa_bits])
}
```

**Test:**

```rego
rsa_key(bits) := {"resource_changes": [{
	"address": "tls_private_key.k",
	"type": "tls_private_key",
	"change": {"actions": ["create"], "after": {"algorithm": "RSA", "rsa_bits": bits}},
}]}

test_rsa_2048_denied if {
	deny_rsa == {"tls_private_key.k: an RSA key needs 4096 bits, and this one has 2048"} with input as rsa_key(2048)
}

test_rsa_4096_allowed if {
	count(deny_rsa) == 0 with input as rsa_key(4096)
}
```

**What Writ does:** Denies the plan at the plan point (`status = "refused"`, no saved plan stored) and refuses apply if added after a plan was created.

## 2. Forbid deletions in `prod` and warn elsewhere (`deletes.rego`)

**Goal:** Refuse any plan that deletes a resource in `prod`, while warning on deletions in non-production environments.

**Rego:**

```rego
package main

import rego.v1

deleted contains rc if {
	some rc in input.resource_changes
	rc.change.actions == ["delete"]
}

deny_delete contains msg if {
	environment == "prod"
	some rc in deleted
	msg := sprintf("%s: a plan deletes nothing in prod; remove the object from the code in a change of its own, reviewed by the owning team", [rc.address])
}

warn_delete contains msg if {
	environment != "prod"
	some rc in deleted
	msg := sprintf("%s is deleted", [rc.address])
}
```

**Test:**

```rego
deletion(env) := {
	"resource_changes": [{"address": "random_pet.p", "type": "random_pet", "change": {"actions": ["delete"]}}],
	"writ": {"environment": env, "decision": "plan"},
}

test_delete_in_prod_denied if {
	count(deny_delete) == 1 with input as deletion("prod")
}

test_delete_elsewhere_warns if {
	count(deny_delete) == 0 with input as deletion("staging")
	count(warn_delete) == 1 with input as deletion("staging")
}
```

**What Writ does:** In `prod`, refuses the plan at the plan point. In other environments (or under `conftest`, where `environment` is `""`), records a `warn` message on the plan without blocking approval or apply.

## 3. Protect stateful resources from replacement (`replace.rego`)

**Goal:** Refuse plans that replace (`["delete", "create"]`) protected resource types, and warn when any other resource is replaced.

**Rego:**

```rego
package main

import rego.v1

protected := {"tls_private_key", "nomad_job"}

replaced contains rc if {
	some rc in input.resource_changes
	"delete" in rc.change.actions
	"create" in rc.change.actions
}

deny_replace contains msg if {
	some rc in replaced
	rc.type in protected
	msg := sprintf("%s: a %s is never replaced by a plan; change the code so OpenTofu updates it in place", [rc.address, rc.type])
}

warn_replace contains msg if {
	some rc in replaced
	not rc.type in protected
	msg := sprintf("%s is replaced: OpenTofu deletes it and creates it again", [rc.address])
}
```

**Test:**

```rego
test_replace_of_nomad_job_denied if {
	count(deny_replace) == 1 with input as {"resource_changes": [{
		"address": "nomad_job.api",
		"type": "nomad_job",
		"change": {"actions": ["delete", "create"]},
	}]}
}
```

**What Writ does:** Refuses any plan that replaces a protected type at the plan point; records a warning for other replacements so reviewers see the destroy-and-recreate in the UI and CLI.

## 4. Block production applies during business hours (`window.rego`)

**Goal:** Allow plans at any hour, but refuse `writ apply --remote` on `prod` between 09:00 and 16:00 local time on weekdays.

**Rego:**

```rego
package main

import rego.v1

zone := "Asia/Kolkata"

deny_window contains msg if {
	decision == "apply"
	environment == "prod"
	now := time.parse_rfc3339_ns(input.writ.now)
	time.weekday([now, zone]) in {"Monday", "Tuesday", "Wednesday", "Thursday", "Friday"}
	[hour, _, _] := time.clock([now, zone])
	hour >= 9
	hour < 16
	msg := sprintf("no applies to prod from 09:00 to 16:00 %s, Monday to Friday", [zone])
}
```

**Test:**

```rego
apply_at(now) := {"resource_changes": [], "writ": {"environment": "prod", "decision": "apply", "now": now}}

test_window_denies_wednesday_morning if {
	count(deny_window) == 1 with input as apply_at("2026-11-11T03:30:00Z")
}

test_window_allows_saturday if {
	count(deny_window) == 0 with input as apply_at("2026-11-14T04:30:00Z")
}
```

**What Writ does:** At the plan point (`decision == "plan"`), the rule does not fire, so engineers can plan and approve changes during the day. At the apply point (`decision == "apply"`), `POST /v1/runs` returns HTTP 403 during the blocked window, leaves the plan approved, and does not spend the CI token.

## 5. Warn on overly permissive local file modes (`files.rego`)

**Goal:** Warn when a `local_file` or `local_sensitive_file` is created with permissions broader than `0600`.

**Rego:**

```rego
package main

import rego.v1

warn_file_permission contains msg if {
	some rc in input.resource_changes
	rc.type in {"local_file", "local_sensitive_file"}
	"create" in rc.change.actions
	rc.change.after.file_permission != "0600"
	msg := sprintf("%s: file_permission is %s; set 0600, so only its owner reads it", [rc.address, rc.change.after.file_permission])
}
```

**Test:**

```rego
test_file_permission_warns if {
	plan := {"resource_changes": [{
		"address": "local_file.cfg",
		"type": "local_file",
		"change": {"actions": ["create"], "after": {"file_permission": "0777"}},
	}]}
	count(warn_file_permission) == 1 with input as plan
}
```

**What Writ does:** Attaches a warning to the plan at both decision points (or denies if you rename `warn_file_permission` to `deny_file_permission`).

## 6. Require `Owner` and `Environment` tags on AWS resources (`cookbook.rego`)

**Goal:** Refuse creation or update of taggable AWS resources (`aws_instance`, `aws_s3_bucket`, `aws_db_instance`, `aws_ebs_volume`) that omit `Owner` or `Environment` tags.

**Rego:**

```rego
package main

import rego.v1

tagged_aws_types := {"aws_db_instance", "aws_ebs_volume", "aws_instance", "aws_s3_bucket"}

required_tags := {"Environment", "Owner"}

resource_tags(rc) := tags if {
	tags := object.get(rc.change.after, "tags_all", null)
	is_object(tags)
} else := tags if {
	tags := object.get(rc.change.after, "tags", null)
	is_object(tags)
} else := {}

deny_missing_tags contains msg if {
	some rc in created_or_updated
	rc.type in tagged_aws_types
	tags := resource_tags(rc)
	some key in required_tags
	object.get(tags, key, "") == ""
	msg := sprintf("%s: missing required tag %q", [rc.address, key])
}
```

**Test:**

```rego
test_missing_tags_denied if {
	plan := {"resource_changes": [{
		"address": "aws_s3_bucket.logs",
		"type": "aws_s3_bucket",
		"change": {"actions": ["create"], "after": {"tags": {"Owner": "platform"}}},
	}]}
	deny_missing_tags == {`aws_s3_bucket.logs: missing required tag "Environment"`} with input as plan
}

test_complete_tags_allowed if {
	plan := {"resource_changes": [{
		"address": "aws_s3_bucket.logs",
		"type": "aws_s3_bucket",
		"change": {"actions": ["create"], "after": {"tags_all": {"Owner": "platform", "Environment": "prod"}}},
	}]}
	count(deny_missing_tags) == 0 with input as plan
}
```

**What Writ does:** Checks `tags_all` (which includes provider `default_tags`) before falling back to resource `tags`, and refuses the plan at the plan point when either required tag is missing or empty.

## 7. Block public S3 buckets (`cookbook.rego`)

**Goal:** Require all four public-access-block flags on `aws_s3_bucket_public_access_block` and forbid public canned ACLs on `aws_s3_bucket_acl`.

**Rego:**

```rego
package main

import rego.v1

s3_public_flags := {
	"block_public_acls",
	"block_public_policy",
	"ignore_public_acls",
	"restrict_public_buckets",
}

deny_public_s3 contains msg if {
	some rc in created_or_updated
	rc.type == "aws_s3_bucket_public_access_block"
	some flag in s3_public_flags
	object.get(rc.change.after, flag, false) != true
	msg := sprintf("%s: %s must be true", [rc.address, flag])
}

deny_public_s3 contains msg if {
	some rc in created_or_updated
	rc.type == "aws_s3_bucket_acl"
	object.get(rc.change.after, "acl", "") in {"authenticated-read", "public-read", "public-read-write"}
	msg := sprintf("%s: acl is %q; S3 buckets may not use a public ACL", [rc.address, rc.change.after.acl])
}
```

**Test:**

```rego
test_public_s3_denied if {
	plan := {"resource_changes": [{
		"address": "aws_s3_bucket_public_access_block.logs",
		"type": "aws_s3_bucket_public_access_block",
		"change": {"actions": ["create"], "after": {
			"block_public_acls": true,
			"block_public_policy": true,
			"ignore_public_acls": true,
			"restrict_public_buckets": false,
		}},
	}]}
	deny_public_s3 == {"aws_s3_bucket_public_access_block.logs: restrict_public_buckets must be true"} with input as plan
}
```

**What Writ does:** Refuses the plan at the plan point before any bucket policy or ACL can be applied.

## 8. Require encryption at rest on EBS and RDS (`cookbook.rego`)

**Goal:** Refuse unencrypted `aws_ebs_volume` and `aws_db_instance` resources.

**Rego:**

```rego
package main

import rego.v1

deny_unencrypted_storage contains msg if {
	some rc in created_or_updated
	rc.type == "aws_ebs_volume"
	object.get(rc.change.after, "encrypted", false) != true
	msg := sprintf("%s: EBS volume must set encrypted = true", [rc.address])
}

deny_unencrypted_storage contains msg if {
	some rc in created_or_updated
	rc.type == "aws_db_instance"
	object.get(rc.change.after, "storage_encrypted", false) != true
	msg := sprintf("%s: RDS instance must set storage_encrypted = true", [rc.address])
}
```

**Test:**

```rego
test_unencrypted_storage_denied if {
	plan := {"resource_changes": [
		{
			"address": "aws_ebs_volume.data",
			"type": "aws_ebs_volume",
			"change": {"actions": ["create"], "after": {"encrypted": false}},
		},
		{
			"address": "aws_db_instance.primary",
			"type": "aws_db_instance",
			"change": {"actions": ["create"], "after": {"storage_encrypted": true}},
		},
	]}
	deny_unencrypted_storage == {"aws_ebs_volume.data: EBS volume must set encrypted = true"} with input as plan
}
```

**What Writ does:** Refuses the plan at the plan point so unencrypted disks or databases cannot be created.

## 9. Forbid `0.0.0.0/0` or `::/0` ingress on SSH and RDP (`cookbook.rego`)

**Goal:** Refuse security group ingress rules that open port 22 (SSH) or 3389 (RDP) to `0.0.0.0/0` or `::/0`.

**Rego:**

```rego
package main

import rego.v1

restricted_ports := {22, 3389}

open_cidr(after) if {
	object.get(after, "cidr_ipv4", "") == "0.0.0.0/0"
}

open_cidr(after) if {
	object.get(after, "cidr_ipv6", "") == "::/0"
}

deny_open_ingress contains msg if {
	some rc in created_or_updated
	rc.type == "aws_vpc_security_group_ingress_rule"
	open_cidr(rc.change.after)
	some port in restricted_ports
	rc.change.after.from_port <= port
	rc.change.after.to_port >= port
	msg := sprintf("%s: opens port %d to the public internet", [rc.address, port])
}
```

**Test:**

```rego
test_open_ssh_ingress_denied if {
	plan := {"resource_changes": [{
		"address": "aws_vpc_security_group_ingress_rule.ssh",
		"type": "aws_vpc_security_group_ingress_rule",
		"change": {"actions": ["create"], "after": {
			"cidr_ipv4": "0.0.0.0/0",
			"from_port": 22,
			"to_port": 22,
		}},
	}]}
	deny_open_ingress == {"aws_vpc_security_group_ingress_rule.ssh: opens port 22 to the public internet"} with input as plan
}
```

**What Writ does:** Refuses the plan at the plan point, including port ranges (such as `0` to `65535`) that span port 22 or 3389.

## 10. Cap EC2 instance size by environment (`cookbook.rego`)

**Goal:** Limit non-production environments to `nano`, `micro`, `small`, or `medium` instances, and cap `prod` at `2xlarge`.

**Rego:**

```rego
package main

import rego.v1

non_prod_sizes := {"nano", "micro", "small", "medium"}

prod_sizes := {"nano", "micro", "small", "medium", "large", "xlarge", "2xlarge"}

instance_size(instance_type) := parts[1] if {
	parts := split(instance_type, ".")
	count(parts) == 2
}

deny_instance_size contains msg if {
	environment != "prod"
	some rc in created_or_updated
	rc.type == "aws_instance"
	it := object.get(rc.change.after, "instance_type", "")
	not instance_size(it) in non_prod_sizes
	msg := sprintf("%s: instance_type %q is too large outside prod; use nano, micro, small, or medium", [rc.address, it])
}

deny_instance_size contains msg if {
	environment == "prod"
	some rc in created_or_updated
	rc.type == "aws_instance"
	it := object.get(rc.change.after, "instance_type", "")
	not instance_size(it) in prod_sizes
	msg := sprintf("%s: instance_type %q exceeds the 2xlarge ceiling in prod", [rc.address, it])
}
```

**Test:**

```rego
test_instance_size_by_environment if {
	large_in_staging := {
		"resource_changes": [{
			"address": "aws_instance.web",
			"type": "aws_instance",
			"change": {"actions": ["create"], "after": {"instance_type": "m7i.xlarge"}},
		}],
		"writ": {"environment": "staging", "decision": "plan"},
	}
	count(deny_instance_size) == 1 with input as large_in_staging
}
```

**What Writ does:** Evaluates `input.writ.environment` at the plan point and refuses oversized instances before approval.

## 11. Restrict resources to approved AWS regions (`cookbook.rego`)

**Goal:** Refuse subnets, EBS volumes, or EC2 instances placed in availability zones outside `eu-west-1` or `us-east-1`.

**Rego:**

```rego
package main

import rego.v1

approved_regions := {"eu-west-1", "us-east-1"}

in_approved_region(az) if {
	some prefix in approved_regions
	startswith(az, prefix)
}

deny_region contains msg if {
	some rc in created_or_updated
	rc.type in {"aws_ebs_volume", "aws_instance", "aws_subnet"}
	az := object.get(rc.change.after, "availability_zone", "")
	az != ""
	not in_approved_region(az)
	msg := sprintf("%s: availability_zone %q is outside approved regions (eu-west-1, us-east-1)", [rc.address, az])
}
```

**Test:**

```rego
test_region_constraint if {
	bad_az := {"resource_changes": [{
		"address": "aws_subnet.a",
		"type": "aws_subnet",
		"change": {"actions": ["create"], "after": {"availability_zone": "ap-south-1a"}},
	}]}
	count(deny_region) == 1 with input as bad_az
}
```

**What Writ does:** Refuses the plan at the plan point when a resource targets an unapproved availability zone.

## 12. Warn on IAM wildcard actions and require two approvals at apply (`cookbook.rego`)

**Goal:** Warn at plan time when an IAM policy grants `"Action": "*"`, and at apply time in `prod`, require at least two approvals on any plan that modifies IAM resources.

**Rego:**

```rego
package main

import rego.v1

iam_types := {"aws_iam_policy", "aws_iam_role", "aws_iam_role_policy", "aws_iam_role_policy_attachment"}

warn_iam_wildcard contains msg if {
	some rc in created_or_updated
	rc.type in {"aws_iam_policy", "aws_iam_role_policy"}
	policy_json := object.get(rc.change.after, "policy", "")
	regex.match(`"Action"\s*:\s*(\[\s*)?"\*"`, policy_json)
	msg := sprintf("%s: IAM policy grants wildcard Action \"*\"", [rc.address])
}

touches_iam if {
	some rc in input.resource_changes
	rc.type in iam_types
	rc.change.actions != ["no-op"]
	rc.change.actions != ["read"]
}

deny_iam_prod_apply contains msg if {
	decision == "apply"
	environment == "prod"
	touches_iam
	count(input.writ.approvals) < 2
	msg := sprintf("IAM changes in prod require at least 2 approvals before apply (have %d)", [count(input.writ.approvals)])
}
```

**Test:**

```rego
test_iam_prod_apply_requires_two_approvals if {
	one_approval := {
		"resource_changes": [{
			"address": "aws_iam_role.runner",
			"type": "aws_iam_role",
			"change": {"actions": ["update"]},
		}],
		"writ": {
			"environment": "prod",
			"decision": "apply",
			"approvals": [{"person": "github/user/1", "display": "alice", "via": "ui"}],
		},
	}
	count(deny_iam_prod_apply) == 1 with input as one_approval
}
```

**What Writ does:** At the plan point, `warn_iam_wildcard` flags wildcard IAM policies for reviewers while allowing the plan to be saved. At the apply point in `prod`, `deny_iam_prod_apply` inspects `input.writ.approvals` and refuses `POST /v1/runs` with HTTP 403 until at least two distinct approvals are recorded.

## Next steps

- [Write and test a policy](write-a-policy.md): step-by-step tutorial from writing a rule to loading it into `writ serve`.
- [Policy](policy.md): how mandatory and advisory sets work at plan and apply time.
- [Policy input and built-ins](policy-reference.md): complete schema for `input` and `input.writ`.
