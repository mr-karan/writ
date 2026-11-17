package server

import (
	"maps"
	"net/http"
	"slices"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/diag"
)

// FormatPolicy is GET /v1/policy.
const FormatPolicy = "writ.policy/v0"

// PolicyDoc is GET /v1/policy: the policy sets writ serve judges every
// plan of an OpenTofu stack with, in config order, each with the text of
// its .rego files. It holds no path.
type PolicyDoc struct {
	Format      string          `json:"format"`
	OK          bool            `json:"ok"`
	Sets        []PolicySetInfo `json:"sets"`
	Diagnostics []Diagnostic    `json:"diagnostics"`
}

// PolicySetInfo is one policy set and its files, by name.
type PolicySetInfo struct {
	Name  string           `json:"name"`
	Level string           `json:"level"`
	Files []PolicyFileInfo `json:"files"`
}

// PolicyFileInfo is one .rego file of a set, named relative to the set's
// directory.
type PolicyFileInfo struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// policyDoc is GET /v1/policy. Every set judges every plan, so a
// principal that may read any stack@env, whose plans show the sets'
// messages, may read every set, as it may read the config.
func (s *Server) policyDoc(w http.ResponseWriter, r *http.Request) {
	req := reqOf(r)
	var grants []int
	for _, row := range s.cfg.Policy.Table(req.principal.teams) {
		if slices.Contains(row.Can, authz.Read) || slices.Contains(row.Can, authz.Admin) {
			grants = append(grants, row.Grant)
		}
	}
	req.entry.Op = string(authz.Read)
	if len(grants) == 0 {
		s.refuse(w, r, FormatPolicy, diag.Denied, http.StatusForbidden, "%s may not read the policy: no grant gives %s read on any stack", req.principal.info.Display, teamWords(req.principal.teams))
		return
	}
	req.entry.Decision, req.entry.Grants = control.Allowed, oneBased(grants)
	doc := PolicyDoc{Format: FormatPolicy, OK: true, Sets: []PolicySetInfo{}, Diagnostics: []Diagnostic{}}
	if p := s.cfg.Policies; p != nil {
		for _, set := range p.Sets {
			info := PolicySetInfo{Name: set.Name, Level: string(set.Level), Files: []PolicyFileInfo{}}
			for _, name := range slices.Sorted(maps.Keys(set.Files)) {
				info.Files = append(info.Files, PolicyFileInfo{Name: name, Text: set.Files[name]})
			}
			doc.Sets = append(doc.Sets, info)
		}
	}
	s.reply(w, http.StatusOK, doc)
}
