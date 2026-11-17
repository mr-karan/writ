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
