package main

import rego.v1

# Replacing an object of these types loses what it holds or restarts what
# it runs, so a person replaces it by hand.
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
