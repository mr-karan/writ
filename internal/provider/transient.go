package provider

import "github.com/mr-karan/writ/internal/value"

// ClearTransientState removes provider responses that must never be persisted.
func ClearTransientState(wire string, v value.Value) value.Value {
	if wire != "nomad_job" || v.Kind() != value.Object {
		return v
	}
	fields := make(map[string]value.Value, len(v.Keys()))
	for _, k := range v.Keys() {
		fields[k] = v.Get(k)
		if k == "plan_response" {
			fields[k] = value.NullVal()
		}
	}
	out := value.ObjectVal(fields).WithObservedDigests(v.ObservedDigests())
	if v.IsSecret() {
		out = out.Secret()
	}
	return out
}

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
