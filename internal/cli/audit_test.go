package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mr-karan/writ/internal/server"
)

// writ audit --remote --changes asks GET /v1/audit for the changes only,
// and without it asks for every row, as before.
func TestRemoteAuditChanges(t *testing.T) {
	for _, changes := range []bool{false, true} {
		var query string
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			query = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(server.AuditDoc{Format: server.FormatAudit, OK: true, Rows: []server.AuditRow{}, Diagnostics: []server.Diagnostic{}})
		}))
		var out bytes.Buffer
		a := standInApp(t, s.URL, &out)
		err := a.RemoteAudit(context.Background(), AuditOptions{Limit: 50, Changes: changes})
		s.Close()
		if err != nil {
			t.Fatalf("changes %v: %v", changes, err)
		}
		if got := strings.Contains(query, "changes=true"); got != changes {
			t.Errorf("changes %v: the query is %q", changes, query)
		}
	}
}
