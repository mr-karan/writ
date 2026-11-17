package main

import rego.v1

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

test_delete_without_writ_warns if {
	count(warn_delete) == 1 with input as {"resource_changes": [{"address": "random_pet.p", "change": {"actions": ["delete"]}}]}
}

apply_at(now) := {"resource_changes": [], "writ": {"environment": "prod", "decision": "apply", "now": now}}

test_window_denies_wednesday_morning if {
	count(deny_window) == 1 with input as apply_at("2026-11-11T03:30:00Z")
}

test_window_allows_saturday if {
	count(deny_window) == 0 with input as apply_at("2026-11-14T04:30:00Z")
}

test_replace_of_nomad_job_denied if {
	count(deny_replace) == 1 with input as {"resource_changes": [{"address": "nomad_job.api", "type": "nomad_job", "change": {"actions": ["delete", "create"]}}]}
}
