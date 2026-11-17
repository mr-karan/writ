package schema

import "strings"

// ownershipEntry says which fields of one resource type a runtime usually
// changes. Scaled fields are ones an autoscaler moves, such as an
// autoscaling group's desired_capacity: Writ warns before reverting one and
// import writes them `initial`, but never hands one to the runtime by
// itself, because a fixed-size group is legitimate code ownership.
type ownershipEntry struct {
	Scaled []string
}

// ownershipTable is the ownership part of the annotation layer, keyed by
// provider source and wire type name.
func ownershipTable() map[Source]map[string]ownershipEntry {
	return map[Source]map[string]ownershipEntry{
		{Namespace: "hashicorp", Type: "aws"}: {
			"aws_autoscaling_group": {Scaled: []string{"desired_capacity"}},
			"aws_ecs_service":       {Scaled: []string{"desired_count"}},
		},
	}
}

// applyOwnership sets r's Scaled from the table. Entries that name an
// attribute this provider version lacks are skipped, as other annotations
// are, because the table covers several versions.
func applyOwnership(src Source, r *Resource) {
	e := ownershipTable()[Source{Namespace: src.Namespace, Type: src.Type}][r.Wire]
	for _, p := range e.Scaled {
		if a, ok := r.Block.Lookup(strings.Split(p, ".")); ok && (a.Optional || a.Required) {
			r.Scaled = append(r.Scaled, p)
		}
	}
}
