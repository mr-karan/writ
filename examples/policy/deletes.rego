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
