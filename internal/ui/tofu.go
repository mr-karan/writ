package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/server"
)

// tofuView is the masked document of an OpenTofu plan as the plan page
// shows it: the counts of OpenTofu's summary, the resource changes that
// change something, grouped by module, the drift, and the outputs that
// change. Every value comes from the masked document, so a value under a
// sensitive mark is the mask's (sensitive).
type tofuView struct {
	Version                                       string
	Add, Change, Destroy, Replace, Forget, Import int
	// Total counts the resource changes that change something.
	Total int
	// Modules are the changes of the page shown, by module.
	Modules []tofuModule
	// Unchanged counts the resources the plan leaves as they are.
	Unchanged        int
	Drift            []tofuChangeView
	Outputs          []tofuOutputView
	UnchangedOutputs int
}

type tofuModule struct {
	// Module is the module's address, "" for the root module.
	Module  string
	Changes []tofuChangeView
}

type tofuChangeView struct {
	Anchor, Module, Addr, Kind, Sign, Actions string
	// Drift is set for a change found outside OpenTofu.
	Drift              bool
	Reason, ReasonText string
	Risk               []string
	Previous, Deposed  string
	// Importing is set for a change that imports an object, ImportID to
	// the id it imports.
	Importing    bool
	ImportID     string
	ReplacePaths []string
	// HasBefore and HasAfter say whether the change has an object before
	// and after it: a create has none before, a delete none after.
	HasBefore, HasAfter bool
	// Secret is set when a value under a sensitive mark changes.
	Secret bool
	// Notes is set when an attribute carries a note: it forces a replace,
	// or a value under a sensitive mark changes. The table of attributes
	// has a column of notes only then.
	Notes bool
	// Open is set for a change the page shows unfolded.
	Open bool
	// Attrs are the attributes the change sets to a new value, Same the
	// ones it leaves as they are.
	Attrs, Same []tofuAttrView
}

type tofuAttrView struct {
	Name, Before, After string
	// Pretty are the values formatted over lines, when they are long.
	BeforePretty, AfterPretty string
	Forces                    bool
	// SecretChanges is set when a value under a sensitive mark differs
	// between before and after, which both show as (sensitive).
	SecretChanges bool
}

// valueView is one value as a page sets it: on one line, and for a long
// one folded, with Pretty under it when it formats over lines.
type valueView struct {
	V, Pretty string
	// Long is set for a value the page folds to one line until a reader
	// opens it.
	Long bool
}

// longValue is the length past which a value is folded: about two lines
// of a column of the table of attributes.
const longValue = 96

func newValueView(v, pretty string) valueView {
	if !strings.Contains(pretty, "\n") {
		pretty = ""
	}
	return valueView{V: v, Pretty: pretty, Long: pretty != "" || len(v) > longValue}
}

// unfoldedChanges is the most changes a plan page shows unfolded. A page
// of more lists them folded, one row a change.
const unfoldedChanges = 5

type tofuOutputView struct {
	Name, Kind, Sign, Before, After string
	SecretChanges                   bool
}

// reasonText words OpenTofu's action_reason.
var reasonText = map[string]string{
	"replace_because_tainted":           "the object is tainted",
	"replace_because_cannot_update":     "the provider cannot update it in place",
	"replace_by_request":                "the plan was asked to replace it",
	"replace_by_triggers":               "replace_triggered_by names a change",
	"delete_because_no_resource_config": "the code no longer declares it",
	"delete_because_wrong_repetition":   "it switched between count and for_each",
	"delete_because_count_index":        "its count index is out of range",
	"delete_because_each_key":           "its for_each key is gone",
	"delete_because_no_module":          "its module is gone",
	"delete_because_no_move_target":     "its moved block has no target",
	"read_because_config_unknown":       "its configuration is known only after apply",
	"read_because_dependency_pending":   "it depends on a change not applied yet",
	"read_because_check_nested":         "a check block reads it",
}

// readTofuPlan reads the masked document of an OpenTofu plan. Numbers stay
// as written, so a large integer prints exactly.
func readTofuPlan(raw json.RawMessage) (*mask.Plan, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var p mask.Plan
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

// newTofuView sorts and words an OpenTofu plan's changes for review. It
// fills v's filters, counts, and page.
// An action list writ serve does not know is an error, as it is when the
// plan is made.
func newTofuView(p *mask.Plan, v *planView) (*tofuView, error) {
	t := &tofuView{Version: p.TerraformVersion}
	type entry struct {
		c    mask.ResourceChange
		kind string
		risk []string
	}
	var all []entry
	for _, c := range p.ResourceChanges {
		kind, risk, err := server.TofuChange(c)
		if err != nil {
			return nil, err
		}
		if kind == "" {
			t.Unchanged++
			continue
		}
		all = append(all, entry{c, kind, risk})
		switch kind {
		case "create":
			t.Add++
		case "update":
			t.Change++
		case "delete":
			t.Destroy++
		case "replace":
			t.Replace++
		case "forget":
			t.Forget++
		}
		if c.Change.Importing != nil {
			t.Import++
		}
		if !slices.Contains(v.Actions, kind) {
			v.Actions = append(v.Actions, kind)
		}
		for _, r := range risk {
			if !slices.Contains(v.Risks, r) {
				v.Risks = append(v.Risks, r)
			}
		}
	}
	t.Total = len(all)
	slices.Sort(v.Actions)
	slices.Sort(v.Risks)
	slices.SortStableFunc(all, func(a, b entry) int {
		if a.c.ModuleAddress != b.c.ModuleAddress {
			return strings.Compare(a.c.ModuleAddress, b.c.ModuleAddress)
		}
		return strings.Compare(a.c.Address, b.c.Address)
	})
	var shown []tofuChangeView
	for i, e := range all {
		if (v.Filter.Action == "" || e.kind == v.Filter.Action) && (v.Filter.Risk == "" || slices.Contains(e.risk, v.Filter.Risk)) {
			shown = append(shown, tofuChange(e.c, e.kind, e.risk, "change-"+strconv.Itoa(i+1)))
		}
	}
	v.Shown = len(shown)
	v.Pages = max(1, (len(shown)+changesPerPage-1)/changesPerPage)
	v.Page = min(max(v.Page, 1), v.Pages)
	start := (v.Page - 1) * changesPerPage
	listed := shown[start:min(start+changesPerPage, len(shown))]
	for _, c := range listed {
		c.Open = len(listed) <= unfoldedChanges
		if n := len(t.Modules); n == 0 || t.Modules[n-1].Module != c.Module {
			t.Modules = append(t.Modules, tofuModule{Module: c.Module})
		}
		t.Modules[len(t.Modules)-1].Changes = append(t.Modules[len(t.Modules)-1].Changes, c)
	}
	for i, c := range p.ResourceDrift {
		kind, _, err := server.TofuChange(c)
		if err != nil {
			return nil, err
		}
		cv := tofuChange(c, kind, nil, "drift-"+strconv.Itoa(i+1))
		cv.Drift = true
		t.Drift = append(t.Drift, cv)
	}
	for _, name := range slices.Sorted(maps.Keys(p.OutputChanges)) {
		c := p.OutputChanges[name]
		kind := outputKind(c.Actions)
		if kind == "" {
			t.UnchangedOutputs++
			continue
		}
		before, after := valueOf(c.Before), valueOf(c.After)
		switch kind {
		case "create":
			before = ""
		case "delete":
			after = ""
		}
		if c.AfterUnknown == true {
			after = "(known after apply)"
		}
		t.Outputs = append(t.Outputs, tofuOutputView{Name: name, Kind: kind, Sign: sign(kind), Before: before, After: after, SecretChanges: len(c.ChangedSensitivePaths) > 0})
	}
	return t, nil
}

func outputKind(actions []string) string {
	switch strings.Join(actions, ",") {
	case "create":
		return "create"
	case "update":
		return "update"
	case "delete":
		return "delete"
	case "delete,create", "create,delete":
		return "replace"
	}
	return ""
}

// tofuChange words one resource change, of the plan or of its drift.
func tofuChange(c mask.ResourceChange, kind string, risk []string, anchor string) tofuChangeView {
	cv := tofuChangeView{Anchor: anchor, Module: c.ModuleAddress, Addr: c.Address, Kind: kind, Sign: sign(kind), Actions: strings.Join(c.Change.Actions, ", "),
		Reason: c.ActionReason, ReasonText: reasonText[c.ActionReason], Risk: risk, Previous: c.PreviousAddress, Deposed: c.Deposed}
	if kind == "" {
		cv.Kind = cv.Actions
	}
	cv.HasBefore, cv.HasAfter = c.Change.Before != nil, c.Change.After != nil
	if c.Change.Importing != nil {
		cv.Importing, cv.ImportID = true, c.Change.Importing.ID
	}
	forces := map[string]bool{}
	for _, p := range c.Change.ReplacePaths {
		cv.ReplacePaths = append(cv.ReplacePaths, pathText(p))
		if len(p) > 0 {
			forces[fmt.Sprint(p[0])] = true
		}
	}
	secret := map[string]bool{}
	for _, p := range c.Change.ChangedSensitivePaths {
		if len(p) > 0 {
			secret[fmt.Sprint(p[0])] = true
		}
	}
	before, _ := c.Change.Before.(map[string]any)
	after, _ := c.Change.After.(map[string]any)
	unknown, _ := c.Change.AfterUnknown.(map[string]any)
	names := slices.Sorted(maps.Keys(before))
	for name := range after {
		if _, ok := before[name]; !ok {
			names = append(names, name)
		}
	}
	for name := range unknown {
		if _, ok := before[name]; !ok {
			if _, ok := after[name]; !ok {
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	for _, name := range names {
		b, inBefore := before[name]
		a, inAfter := after[name]
		if b == nil && a == nil && !holdsTrue(unknown[name]) {
			continue
		}
		av := tofuAttrView{Name: name, Forces: forces[name], SecretChanges: secret[name]}
		cv.Secret = cv.Secret || av.SecretChanges
		if c.Change.Before != nil && inBefore {
			av.Before, av.BeforePretty = valueOf(b), prettyValue(b)
		}
		switch u := unknown[name]; {
		case u == true:
			av.After = "(known after apply)"
		case c.Change.After != nil && inAfter:
			av.After, av.AfterPretty = valueOf(a), prettyValue(a)
			if holdsTrue(u) {
				av.After += " (parts known after apply)"
			}
		}
		changed := holdsTrue(unknown[name]) || av.SecretChanges || c.Change.Before == nil || c.Change.After == nil ||
			inBefore != inAfter || !reflect.DeepEqual(b, a)
		if changed {
			cv.Notes = cv.Notes || av.Forces || av.SecretChanges
			cv.Attrs = append(cv.Attrs, av)
		} else {
			cv.Same = append(cv.Same, av)
		}
	}
	return cv
}

// holdsTrue reports whether a mark tree, such as after_unknown, marks
// anything.
func holdsTrue(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case map[string]any:
		for _, x := range v {
			if holdsTrue(x) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(v, holdsTrue)
	}
	return false
}

// valueOf prints a value of the masked document on one line: a string
// quoted, the mask's marker bare, and anything else as compact JSON.
func valueOf(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case string:
		if v == mask.Sensitive {
			return v
		}
		return strconv.Quote(v)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return "(unreadable)"
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// prettyValue formats a long object or list over lines, or a string that
// holds a JSON object or list, such as a policy. It returns "" for any
// other value.
func prettyValue(v any) string {
	switch x := v.(type) {
	case string:
		raw, _ := json.Marshal(x)
		return prettyJSON(raw)
	case map[string]any, []any:
		one := valueOf(v)
		if len(one) <= 80 {
			return ""
		}
		var b bytes.Buffer
		if json.Indent(&b, []byte(one), "", "  ") != nil {
			return ""
		}
		return b.String()
	}
	return ""
}

// pathText writes an attribute path of a plan, such as keepers or
// rule[0].port.
func pathText(path []any) string {
	var b strings.Builder
	for i, step := range path {
		switch v := step.(type) {
		case string:
			if i > 0 {
				b.WriteByte('.')
			}
			b.WriteString(v)
		default:
			fmt.Fprintf(&b, "[%v]", v)
		}
	}
	return b.String()
}

// isTofuPlan reports whether a plan's document is the masked document of
// an OpenTofu plan, which has a format_version.
func isTofuPlan(raw json.RawMessage) bool {
	var head struct {
		FormatVersion string `json:"format_version"`
	}
	return json.Unmarshal(raw, &head) == nil && head.FormatVersion != ""
}

// tofuRunData is the page of a run that applied an OpenTofu plan.
type tofuRunData struct {
	Run  *server.RunInfo
	Plan *server.PlanInfo
	// Doc is writ.tofu-apply/v0, nil while the run applies.
	Doc       *server.TofuApply
	DocError  string
	Resources []tofuRunResource
	Took      string
	// TookFor is Took as an ISO 8601 duration, for its time element.
	TookFor string
	Text    string
	Steps   []step
}

// tofuRunResource is what the apply did to one resource, from its events.
type tofuRunResource struct {
	Addr, Action, Outcome string
	Elapsed               int
}

// newTofuRun reads a run's document and folds its events by resource and
// action, in the order the apply started them: a replace deletes and
// creates one address.
func newTofuRun(rr server.RemoteRun, plan *server.PlanInfo) tofuRunData {
	d := tofuRunData{Run: rr.Run, Plan: plan, Text: rr.Text, Steps: runSteps(rr.Run)}
	d.Took, d.TookFor = took(rr.Run.StartedAt, rr.Run.EndedAt)
	if len(rr.Document) == 0 || string(rr.Document) == "null" {
		return d
	}
	var doc server.TofuApply
	if err := json.Unmarshal(rr.Document, &doc); err != nil {
		d.DocError = err.Error()
		return d
	}
	d.Doc = &doc
	at := map[[2]string]int{}
	for _, ev := range doc.Events {
		key := [2]string{ev.Address, ev.Action}
		i, ok := at[key]
		if !ok {
			i = len(d.Resources)
			at[key] = i
			d.Resources = append(d.Resources, tofuRunResource{Addr: ev.Address, Action: ev.Action, Outcome: "started, not finished"})
		}
		switch ev.Type {
		case "apply_complete":
			d.Resources[i].Outcome, d.Resources[i].Elapsed = "complete", ev.ElapsedSeconds
		case "apply_errored":
			d.Resources[i].Outcome, d.Resources[i].Elapsed = "errored", ev.ElapsedSeconds
		}
	}
	return d
}
