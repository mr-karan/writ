package ui_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/notify"
)

// TestSettingsShowsNotifiers follows the link of an approval-needed
// notification to the plan's page, then holds the Settings page's
// Notifications card to each notifier, its events, its counts, and its last
// error, by the rules of the design, with no URL or secret on it.
func TestSettingsShowsNotifiers(t *testing.T) {
	controltest.Each(t, func(t *testing.T, e controltest.Engine) {
		got := make(chan notify.Event, 8)
		sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var ev notify.Event
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &ev)
			got <- ev
		}))
		defer sink.Close()
		dir := t.TempDir()
		write := func(name, text string) string {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}
		const hookPath = "/hooks/NOTIFY_URL_SECRET"
		const signing = "notify-signing-secret-77"
		f := newFixtureOn(t, e, fmt.Sprintf(`
notify "ops-webhook" {
  kind        = "webhook"
  url_file    = %q
  secret_file = %q
  events      = ["approval-needed", "drift"]
  envs        = ["prod"]
}

notify "pager" {
  kind     = "slack"
  url_file = %q
  events   = ["apply-failed"]
}
`, write("hook_url", sink.URL+hookPath), write("hook_secret", signing), write("pager_url", "https://hooks.example.com/services/T000/B000/PAGER_URL_SECRET")))

		id := f.plan(nil)
		var ev notify.Event
		select {
		case ev = <-got:
		case <-time.After(10 * time.Second):
			t.Fatal("no approval-needed notification came")
		}
		if ev.Event != notify.EventApprovalNeeded || ev.URL != f.url+"/ui/plans/"+id {
			t.Fatalf("notification %+v, want approval-needed linking plan %s", ev, id)
		}
		b := f.browser()
		b.signIn("/ui/")
		if p := b.get(strings.TrimPrefix(ev.URL, f.url)); p.status != http.StatusOK || !strings.Contains(text(p.doc), short(id)) {
			t.Fatalf("the notification's link answered %d", p.status)
		}

		at := time.Now().Add(-time.Hour)
		if err := f.store.RecordNotification(context.Background(), control.NotificationResult{Name: "pager", Event: "apply-failed", Failed: 1, Err: "http status 502", At: at}); err != nil {
			t.Fatal(err)
		}
		var session string
		for _, c := range b.c.Jar.Cookies(mustURL(f.url + "/ui/")) {
			if c.Name == "__Host-writ_session" {
				session = c.Value
			}
		}
		p := b.get("/ui/settings")
		if p.status != http.StatusOK {
			t.Fatalf("settings: %d", p.status)
		}
		checkPage(t, p, session)
		var card *html.Node
		each(p.doc, func(n *html.Node) {
			if v, _ := attr(n, "aria-labelledby"); v == "h-notify" {
				card = n
			}
		})
		if card == nil {
			t.Fatalf("no Notifications card: %s", p.body)
		}
		var rows []string
		each(card, func(n *html.Node) {
			if n.Data == "tr" && inside(n, "tbody") {
				rows = append(rows, strings.Join(strings.Fields(text(n)), " "))
			}
		})
		want := []string{
			"ops-webhook webhook approval-needed drift drift on-change 1 0 ",
			"pager slack apply-failed 0 1 not yet failed http status 502 ",
		}
		if len(rows) != len(want) {
			t.Fatalf("rows %q, want %d", rows, len(want))
		}
		for i, row := range rows {
			if !strings.HasPrefix(row, want[i]) {
				t.Errorf("row %d is %q, want it to start %q", i, row, want[i])
			}
		}
		for _, secret := range []string{"NOTIFY_URL_SECRET", "PAGER_URL_SECRET", signing, "hook_url", "pager_url", sink.URL} {
			if strings.Contains(string(p.body), secret) {
				t.Errorf("the Settings page holds %q", secret)
			}
		}
	})
}
