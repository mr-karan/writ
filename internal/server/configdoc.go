package server

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/notify"
)

// FormatConfig is GET /v1/config.
const FormatConfig = "writ.config/v0"

// ConfigDoc is GET /v1/config: the config writ serve runs with, as
// writ serve --check prints it. The config holds no secret, and the
// document holds no path: no key file, no secret file, and no database.
type ConfigDoc struct {
	Format  string       `json:"format"`
	OK      bool         `json:"ok"`
	URL     string       `json:"url"`
	Issuers []IssuerInfo `json:"issuers"`
	Teams   []TeamInfo   `json:"teams"`
	Grants  []GrantInfo  `json:"grants"`
	Envs    []EnvInfo    `json:"envs"`
	// Forges are where repositories are read from. Their token files stay
	// out of the document.
	Forges []ForgeInfo `json:"forges"`
	// Notifiers are the notification targets with their delivery counts and
	// last outcome. Their URL and signing secret files stay out of the
	// document.
	Notifiers []NotifierInfo `json:"notifiers"`
	// PollEvery is how often each repository's default branch is polled,
	// a Go duration.
	PollEvery string `json:"poll_every"`
	// PlanTTL and SessionTTL are Go durations, such as "24h0m0s".
	PlanTTL    string `json:"plan_ttl"`
	SessionTTL string `json:"session_ttl"`
	UI         bool   `json:"ui"`
	// Admin holds the limits and the retention, for a principal with
	// admin; null for every other principal.
	Admin       *AdminConfig `json:"admin"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// ForgeInfo is one forge of the config.
type ForgeInfo struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	URL  string `json:"url"`
}

// NotifierInfo is one configured notifier and its delivery status. It holds
// no webhook URL, signing secret, or file path.
type NotifierInfo struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Events      []string `json:"events"`
	Envs        []string `json:"envs,omitempty"`
	Stacks      []string `json:"stacks,omitempty"`
	DriftMode   string   `json:"drift_mode,omitempty"`
	Delivered   int      `json:"delivered"`
	Failed      int      `json:"failed"`
	LastEvent   string   `json:"last_event"`
	LastOKAt    string   `json:"last_ok_at"`
	LastErrorAt string   `json:"last_error_at"`
	Error       string   `json:"error"`
}

// TeamInfo is one team and its rule.
type TeamInfo struct {
	Name   string      `json:"name"`
	Issuer string      `json:"issuer"`
	Match  []MatchRule `json:"match"`
}

// MatchRule is one claim a team matches, and the values that match.
type MatchRule struct {
	Claim  string   `json:"claim"`
	Values []string `json:"values"`
}

// GrantInfo is one grant, numbered from 1 in config order.
type GrantInfo struct {
	Grant  int      `json:"grant"`
	Teams  []string `json:"teams"`
	Stacks []string `json:"stacks"`
	Envs   []string `json:"envs"`
	Can    []string `json:"can"`
	Risks  []string `json:"risks"`
}

// EnvInfo is one environment.
type EnvInfo struct {
	Name string `json:"name"`
}

// AdminConfig is the part of the config only admin reads.
type AdminConfig struct {
	Limits    LimitsInfo    `json:"limits"`
	Retention RetentionInfo `json:"retention"`
}

// LimitsInfo is the config's limits.
type LimitsInfo struct {
	RequestsPerSecond  int   `json:"requests_per_second"`
	RequestsPerAddress int   `json:"requests_per_address"`
	PlansPerPrincipal  int   `json:"plans_per_principal"`
	Plans              int   `json:"plans"`
	Body               int64 `json:"body_bytes"`
	// PlanTimeout, ApplyStartTimeout, and ApplyTimeout are Go durations.
	PlanTimeout       string `json:"plan_timeout"`
	ApplyStartTimeout string `json:"apply_start_timeout"`
	ApplyTimeout      string `json:"apply_timeout"`
}

// RetentionInfo is how long audit rows and spent plans are kept; "" keeps
// them forever.
type RetentionInfo struct {
	Audit string `json:"audit"`
	Plans string `json:"plans"`
}

// configDoc is GET /v1/config. A principal needs read on some stack@env;
// admin anywhere adds the limits and the retention.
func (s *Server) configDoc(w http.ResponseWriter, r *http.Request) {
	req := reqOf(r)
	var read, admin bool
	var grants []int
	for _, row := range s.cfg.Policy.Table(req.principal.teams) {
		if slices.Contains(row.Can, authz.Read) || slices.Contains(row.Can, authz.Admin) {
			grants = append(grants, row.Grant)
		}
		read = read || slices.Contains(row.Can, authz.Read)
		admin = admin || slices.Contains(row.Can, authz.Admin)
	}
	req.entry.Op = string(authz.Read)
	if !read && !admin {
		s.refuse(w, r, FormatConfig, diag.Denied, http.StatusForbidden, "%s may not read the config: no grant gives %s read on any stack", req.principal.info.Display, teamWords(req.principal.teams))
		return
	}
	req.entry.Decision, req.entry.Grants = control.Allowed, oneBased(grants)
	cfg := s.cfg
	doc := ConfigDoc{
		Format: FormatConfig, OK: true, URL: cfg.URL, Issuers: []IssuerInfo{}, Teams: []TeamInfo{}, Grants: []GrantInfo{},
		Envs: []EnvInfo{}, Forges: []ForgeInfo{}, Notifiers: []NotifierInfo{}, PollEvery: cfg.PollEvery.String(),
		PlanTTL: cfg.PlanTTL.String(), SessionTTL: cfg.SessionTTL.String(), UI: cfg.UI, Diagnostics: []Diagnostic{},
	}
	for _, f := range cfg.Forges {
		doc.Forges = append(doc.Forges, ForgeInfo{Name: f.Name, Kind: string(f.Kind), URL: f.URL})
	}
	if len(cfg.Notifiers) > 0 {
		statuses, err := s.store.NotificationStatuses(r.Context())
		if err != nil {
			s.dbFailed(w, r, FormatConfig, err)
			return
		}
		for _, n := range cfg.Notifiers {
			evs := make([]string, len(n.Events))
			for i, ev := range n.Events {
				evs[i] = string(ev)
			}
			ni := NotifierInfo{Name: n.Name, Kind: string(n.Kind), Events: evs, Envs: n.Envs, Stacks: n.Stacks}
			if slices.Contains(n.Events, notify.EventDrift) {
				ni.DriftMode = string(n.DriftMode)
			}
			if st, ok := statuses[n.Name]; ok {
				ni.Delivered = int(st.Delivered)
				ni.Failed = int(st.Failed)
				ni.LastEvent = st.LastEvent
				ni.LastOKAt = stamp(st.LastOKAt)
				ni.LastErrorAt = stamp(st.LastErrorAt)
				ni.Error = st.LastError
			}
			doc.Notifiers = append(doc.Notifiers, ni)
		}
	}
	for _, is := range cfg.Issuers {
		doc.Issuers = append(doc.Issuers, IssuerInfo{Name: is.Name, Kind: string(is.Kind), URL: is.URL, Identity: is.Identity})
	}
	for _, t := range cfg.Policy.Teams {
		ti := TeamInfo{Name: t.Name, Issuer: t.Issuer, Match: []MatchRule{}}
		for _, m := range t.Match {
			ti.Match = append(ti.Match, MatchRule{Claim: m.Claim, Values: m.Values})
		}
		doc.Teams = append(doc.Teams, ti)
	}
	for i, g := range cfg.Policy.Grants {
		can := make([]string, len(g.Can))
		for j, op := range g.Can {
			can[j] = string(op)
		}
		doc.Grants = append(doc.Grants, GrantInfo{Grant: i + 1, Teams: g.Teams, Stacks: g.Stacks, Envs: g.Envs, Can: can, Risks: orEmpty(g.Risks)})
	}
	envs, err := s.environments(r.Context())
	if err != nil {
		s.refuse(w, r, FormatConfig, diag.StateIO, http.StatusServiceUnavailable, "the control database failed; the request id is %s", req.id)
		return
	}
	for _, name := range envs {
		doc.Envs = append(doc.Envs, EnvInfo{Name: name})
	}
	if admin {
		doc.Admin = &AdminConfig{
			Limits: LimitsInfo{RequestsPerSecond: cfg.Limits.RequestsPerSecond, RequestsPerAddress: cfg.Limits.RequestsPerAddress, PlansPerPrincipal: cfg.Limits.PlansPerPrincipal,
				Plans: cfg.Limits.Plans, Body: cfg.Limits.Body,
				PlanTimeout: cfg.Limits.PlanTimeout.String(), ApplyStartTimeout: cfg.Limits.ApplyStartTimeout.String(), ApplyTimeout: cfg.Limits.ApplyTimeout.String()},
			Retention: RetentionInfo{Audit: durationOr(cfg.Retention.Audit), Plans: durationOr(cfg.Retention.Plans)},
		}
	}
	s.reply(w, http.StatusOK, doc)
}

func durationOr(d time.Duration) string {
	if d == 0 {
		return ""
	}
	return d.String()
}

func teamWords(teams []string) string {
	if len(teams) == 0 {
		return "a principal in no team"
	}
	return "the teams " + strings.Join(teams, ", ")
}
