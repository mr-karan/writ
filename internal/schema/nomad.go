package schema

import "github.com/mr-karan/writ/internal/types"

// PatchedNomad reports the job schema's complete patch contract, independent
// of its source address and of the optional sensitivity marker.
func (r *Resource) PatchedNomad() bool {
	if r == nil || r.Wire != "nomad_job" {
		return false
	}
	for _, name := range []string{"plan_response", "register_modify_index", "register_eval_id", "register_eval_create_index"} {
		a, ok := r.Block.Lookup([]string{name})
		if !ok || !a.Computed || a.Optional || a.Required || a.Type.Kind() != types.String {
			return false
		}
	}
	return true
}
