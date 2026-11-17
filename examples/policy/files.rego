package main

import rego.v1

warn_file_permission contains msg if {
	some rc in input.resource_changes
	rc.type in {"local_file", "local_sensitive_file"}
	"create" in rc.change.actions
	rc.change.after.file_permission != "0600"
	msg := sprintf("%s: file_permission is %s; set 0600, so only its owner reads it", [rc.address, rc.change.after.file_permission])
}
