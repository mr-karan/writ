package provider

import "github.com/mr-karan/writ/internal/value"

func (c *Client) priorState(wire string, v value.Value) value.Value {
	if wire != "nomad_job" || c.src.Namespace != "hashicorp" || c.src.Type != "nomad" || c.PatchedNomad() || v.Kind() != value.Object {
		return v
	}
	fields := make(map[string]value.Value, len(v.Keys()))
	for _, k := range v.Keys() {
		switch k {
		case "plan_response", "register_modify_index", "register_eval_id", "register_eval_create_index":
		default:
			fields[k] = v.Get(k)
		}
	}
	out := value.ObjectVal(fields).WithObservedDigests(v.ObservedDigests())
	if v.IsSecret() {
		out = out.Secret()
	}
	return out
}
