package main

import rego.v1

created_or_updated contains rc if {
	some rc in input.resource_changes
	some action in rc.change.actions
	action in {"create", "update"}
}

# 1. Require Owner and Environment tags on AWS resources.
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

# 2. Block public S3 access blocks and public canned ACLs.
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

# 3. Require encryption at rest on EBS volumes and RDS instances.
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

# 4. Forbid 0.0.0.0/0 or ::/0 ingress on SSH (22) and RDP (3389).
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

# 5. Cap EC2 instance size by environment.
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

# 6. Restrict AWS availability zones to approved regions.
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

# 7. Warn on IAM wildcard actions and require two approvals to apply IAM changes in prod.
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
