package main

import rego.v1

zone := "Asia/Kolkata"

# No apply to prod from 09:00 to 16:00 on a weekday. Only writ serve
# knows when an apply starts, so under conftest this rule never denies.
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
