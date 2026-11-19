package ui

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/url"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/server"
)

//go:embed templates/*.html
var templateFS embed.FS

// parseTemplates parses every template into one set of define blocks. No
// function returns one of html/template's trusted types, which mark text
// as safe: every value is escaped by its context, and TestNoTrustedTypes
// forbids those types in this package.
func parseTemplates(u *UI) (*template.Template, error) {
	funcs := template.FuncMap{
		"asset":  u.assets.url,
		"when":   formatTime,
		"since":  u.relative,
		"short":  short,
		"sign":   sign,
		"join":   strings.Join,
		"link":   link,
		"record": record,
		"clock":  clock,
		"ago": func(s string) string {
			if r := u.relative(s); r != "now" {
				return r
			}
			return "just now"
		},
		"deref": func(p *int) int {
			if p == nil {
				return 0
			}
			return *p
		},
		"plans": newPlanTable,
		"took": func(start, end string) span {
			text, iso := took(start, end)
			return span{text, iso}
		},
		"from":     originOf,
		"driftBox": func(d *server.DriftStack, every span) driftBox { return driftBox{d, every} },
		"add":      func(a, b int) int { return a + b },
		"grants": func(caption string, gs []server.GrantInfo, teams bool) grantTable {
			t := grantTable{Caption: caption, Teams: teams}
			for _, g := range gs {
				t.Rows = append(t.Rows, grantRow{Grant: g.Grant, Teams: g.Teams, Envs: g.Envs, Stacks: g.Stacks, Can: g.Can, Risks: g.Risks})
			}
			return t
		},
		"myGrants": func(caption string, gs []server.GrantRow) grantTable {
			t := grantTable{Caption: caption}
			for _, g := range gs {
				t.Rows = append(t.Rows, grantRow{Grant: g.Grant, Envs: g.Envs, Stacks: g.Stacks, Can: g.Can, Risks: g.Risks})
			}
			return t
		},
		"deref64": func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"tone":       tone,
		"live":       server.PlanLive,
		"offer":      offerOf,
		"vclass":     valueClass,
		"val":        newValueView,
		"planName":   planName,
		"planRecord": planRecord,
		"planDoc":    planDoc,
		"sections":   func() []section { return sections },
		"breaks":     breaks,
		"provider":   shortProvider,
		"sub":        func(a, b int) int { return a - b },
		"none":       none,
		"noun":       noun,
		"exact":      exact,
		"figure":     func(n int, floor bool) figure { return figure{n, floor} },
		"of":         func(f figure, one, many string) part { return counted(f, noun(f, one, many)) },
		"iso":        func(seconds int) string { return fmt.Sprintf("PT%dS", seconds) },
		"hasMethod":  func(api string) bool { return strings.HasPrefix(api, "POST ") },
		"envs": func(base, env string, names []string, all bool) envFilter {
			return envFilter{Base: base, Env: env, Envs: names, All: all}
		},
	}
	return template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
}

// envFilter is the data of the env-filter template: links to base for
// each environment, and one for all when All.
type envFilter struct {
	Base, Env string
	Envs      []string
	All       bool
}

// grantTable is the data of the grant-rows template.
type grantTable struct {
	Caption string
	Rows    []grantRow
	// Teams adds the teams column, for the config's grants.
	Teams bool
}

type grantRow struct {
	Grant                           int
	Teams, Envs, Stacks, Can, Risks []string
}

// planTable is the data of the plan-table template. Status, Stack, and
// Approvals show those columns: a list of pending plans needs no status
// and counts their approvals, and a stack's own list needs no stack.
type planTable struct {
	Caption                  string
	Plans                    []server.PlanInfo
	Status, Stack, Approvals bool
}

// newPlanTable is template func plans: the table of plans under caption,
// with the columns cols names, of "status", "stack", and "approvals".
func newPlanTable(caption string, plans []server.PlanInfo, cols ...string) (planTable, error) {
	t := planTable{Caption: caption, Plans: plans}
	for _, c := range cols {
		switch c {
		case "status":
			t.Status = true
		case "stack":
			t.Stack = true
		case "approvals":
			t.Approvals = true
		default:
			return t, fmt.Errorf("the plan table has no column %q", c)
		}
	}
	return t, nil
}

// picker is the data of the pick template: a control that shows Label and
// the current Value, and opens to a list of links, one for each value.
type picker struct {
	Label, Value string
	Items        []pickItem
}

type pickItem struct {
	Name, Href string
	Current    bool
}

// span is how long something took, as text and as an ISO 8601 duration
// for its time element.
type span struct{ Text, ISO string }

// took is how long it was from start to end, two RFC 3339 times, as text
// such as 4 s, 1 min 12 s, or 2 h 3 min, and as an ISO 8601 duration. It
// is "" while end is unknown.
func took(start, end string) (text, iso string) {
	s, err := time.Parse(time.RFC3339, start)
	if err != nil {
		return "", ""
	}
	e, err := time.Parse(time.RFC3339, end)
	if err != nil {
		return "", ""
	}
	d := max(e.Sub(s).Round(time.Second), 0)
	h, m, sec := int(d/time.Hour), int(d%time.Hour/time.Minute), int(d%time.Minute/time.Second)
	switch {
	case h > 0:
		text = fmt.Sprintf("%d h %d min", h, m)
	case m > 0:
		text = fmt.Sprintf("%d min %d s", m, sec)
	default:
		text = fmt.Sprintf("%d s", sec)
	}
	return text, fmt.Sprintf("PT%dS", int(d.Seconds()))
}

// origin is where a plan came from: a project or a repository, and the
// merge request, pipeline, or GitHub run in it, which the templates set
// as an identifier. Field is the key of the origin's document that holds
// Where.
type origin struct{ Where, Field, Kind, ID string }

// originOf is the origin of a plan, nil when it names none.
func originOf(raw json.RawMessage) *origin {
	var o map[string]any
	if json.Unmarshal(raw, &o) != nil {
		return nil
	}
	str := func(k string) string { s, _ := o[k].(string); return s }
	switch {
	case o["merge_request"] != nil:
		return &origin{Where: str("project"), Field: "project", ID: fmt.Sprintf("!%v", o["merge_request"])}
	case str("pipeline") != "":
		return &origin{Where: str("project"), Field: "project", Kind: "pipeline", ID: str("pipeline")}
	case str("run") != "":
		return &origin{Where: str("repository"), Field: "repository", Kind: "run", ID: str("run")}
	}
	return nil
}

// clock is the hour and minute of an RFC 3339 time, in UTC.
func clock(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("15:04 UTC")
}

// formatTime prints an RFC 3339 time as the CLI does, in UTC to the minute.
func formatTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// relative says how far a time is from now, in the largest whole unit.
func (u *UI) relative(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return ""
	}
	d := t.Sub(u.now())
	future := d > 0
	if !future {
		d = -d
	}
	var n string
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		n = fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 48*time.Hour:
		n = fmt.Sprintf("%d h", int(d/time.Hour))
	default:
		n = fmt.Sprintf("%d days", int(d/(24*time.Hour)))
	}
	if future {
		return "in " + n
	}
	return n + " ago"
}

// short is the first eight characters of a plan id or a run id, as pages
// show it. Links carry the whole id.
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// sign is the CLI's sign for a kind of change, which every change prints
// beside its word, so color is never the only sign.
func sign(kind string) string {
	if s := server.ChangeSign(kind); s != "" {
		return s
	}
	return " "
}

// record names a record of a document writ serve sent by its fields, for
// the data-record of an element that shows one of them: the render check
// holds the element's text to that field of that record.
func record(kv ...string) string { return strings.TrimPrefix(link("", kv...), "?") }

// link builds a path with a query from pairs of keys and values, leaving
// out the pairs whose value is empty.
func link(path string, kv ...string) string {
	q := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			q.Set(kv[i], kv[i+1])
		}
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

// prettyJSON formats a string value that holds a JSON object or list, such
// as an IAM policy, for a reader. It returns "" for any other value.
func prettyJSON(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "{") && !strings.HasPrefix(t, "[") {
		return ""
	}
	var b bytes.Buffer
	if json.Indent(&b, []byte(t), "", "  ") != nil {
		return ""
	}
	return b.String()
}

// planName is how a page names a plan: its id, or before it has one, the
// ref of its request.
func planName(p *server.PlanInfo) string {
	if p.ID != "" {
		return p.ID
	}
	return p.Ref
}

// planDoc names the document of a plan, GET /v1/plans/{id}, by the name
// the page shows of its plan.
func planDoc(p *server.PlanInfo) string {
	if p.ID != "" {
		return record("format", server.FormatRemotePlan, "plan.id", p.ID)
	}
	return record("format", server.FormatRemotePlan, "plan.ref", p.Ref)
}

// planRecord names a plan's record by the name the page shows of it.
func planRecord(p *server.PlanInfo) string {
	if p.ID != "" {
		return record("id", p.ID)
	}
	return record("ref", p.Ref)
}

// section is one item of the navigation; a page names the one it is in
// by Key.
type section struct{ Key, Name, Href string }

var sections = []section{
	{"overview", "Overview", "/ui/"},
	{"runs", "Runs", "/ui/runs"},
	{"plans", "Plans", "/ui/plans"},
	{"approvals", "Approvals", "/ui/approvals"},
	{"repos", "Repositories", "/ui/repos"},
	{"access", "Access", "/ui/access"},
	{"settings", "Settings", "/ui/settings"},
}
