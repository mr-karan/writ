package main

import rego.v1

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

test_public_s3_acl_denied if {
	plan := {"resource_changes": [{
		"address": "aws_s3_bucket_acl.logs",
		"type": "aws_s3_bucket_acl",
		"change": {"actions": ["create"], "after": {"acl": "public-read"}},
	}]}
	count(deny_public_s3) == 1 with input as plan
}

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

test_open_https_ingress_allowed if {
	plan := {"resource_changes": [{
		"address": "aws_vpc_security_group_ingress_rule.https",
		"type": "aws_vpc_security_group_ingress_rule",
		"change": {"actions": ["create"], "after": {
			"cidr_ipv4": "0.0.0.0/0",
			"from_port": 443,
			"to_port": 443,
		}},
	}]}
	count(deny_open_ingress) == 0 with input as plan
}

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

	large_in_prod := {
		"resource_changes": [{
			"address": "aws_instance.web",
			"type": "aws_instance",
			"change": {"actions": ["create"], "after": {"instance_type": "m7i.xlarge"}},
		}],
		"writ": {"environment": "prod", "decision": "plan"},
	}
	count(deny_instance_size) == 0 with input as large_in_prod
}

test_region_constraint if {
	bad_az := {"resource_changes": [{
		"address": "aws_subnet.a",
		"type": "aws_subnet",
		"change": {"actions": ["create"], "after": {"availability_zone": "ap-south-1a"}},
	}]}
	count(deny_region) == 1 with input as bad_az

	good_az := {"resource_changes": [{
		"address": "aws_subnet.a",
		"type": "aws_subnet",
		"change": {"actions": ["create"], "after": {"availability_zone": "eu-west-1a"}},
	}]}
	count(deny_region) == 0 with input as good_az
}

test_iam_wildcard_warns if {
	plan := {"resource_changes": [{
		"address": "aws_iam_policy.admin",
		"type": "aws_iam_policy",
		"change": {"actions": ["create"], "after": {
			"policy": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`,
		}},
	}]}
	warn_iam_wildcard == {`aws_iam_policy.admin: IAM policy grants wildcard Action "*"`} with input as plan
}

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

	two_approvals := {
		"resource_changes": [{
			"address": "aws_iam_role.runner",
			"type": "aws_iam_role",
			"change": {"actions": ["update"]},
		}],
		"writ": {
			"environment": "prod",
			"decision": "apply",
			"approvals": [
				{"person": "github/user/1", "display": "alice", "via": "ui"},
				{"person": "github/user/2", "display": "bob", "via": "cli"},
			],
		},
	}
	count(deny_iam_prod_apply) == 0 with input as two_approvals
}
