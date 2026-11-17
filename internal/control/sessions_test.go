package control_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/control/controltest"
)

func TestSessions(t *testing.T) { each(t, testSessions) }

func testSessions(t *testing.T, s *control.Store, u string) {
	ctx := context.Background()
	hash := sha256.Sum256([]byte("writs_token"))
	sess := control.Session{
		ID: "S1", Carrier: control.Browser, Person: "gitlab.com/user/2002", Display: "bob", Issuer: "sso", Subject: "2002",
		Teams: []string{"platform"}, CreatedAt: t0, EndsAt: t0.Add(12 * time.Hour),
	}
	signIn := control.Entry{At: t0, Request: "R1", Kind: "sign-in", Person: sess.Person, Principal: "bob", PrincipalKind: "person", Via: "browser session", Decision: control.Allowed}
	if err := s.CreateSession(ctx, sess, hash[:], signIn); err != nil {
		t.Fatal(err)
	}
	got, err := s.SessionByHash(ctx, hash[:], control.Browser, t0.Add(time.Hour))
	if err != nil || got.ID != "S1" || got.Carrier != control.Browser || got.Teams[0] != "platform" || !got.EndsAt.Equal(sess.EndsAt) {
		t.Fatalf("session %+v, %v", got, err)
	}
	if _, err := s.SessionByHash(ctx, hash[:], control.CLI, t0.Add(time.Hour)); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("a browser session found as a CLI one: %v", err)
	}
	other := sha256.Sum256([]byte("writs_other"))
	if _, err := s.SessionByHash(ctx, other[:], control.Browser, t0); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("an unknown token found %v", err)
	}
	if _, err := s.SessionByHash(ctx, hash[:], control.Browser, t0.Add(12*time.Hour)); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("a session past its end found %v", err)
	}

	raw := controltest.Raw(t, u)
	var seen int64
	if err := raw.QueryRowContext(ctx, "SELECT last_seen_at FROM sessions WHERE id = 'S1'").Scan(&seen); err != nil || seen != t0.Add(time.Hour).UnixMilli() {
		t.Fatalf("last_seen_at %d, %v", seen, err)
	}
	var rows int
	if err := raw.QueryRowContext(ctx, "SELECT count(*) FROM sessions WHERE hash = ? AND id || person || display NOT LIKE '%writs_token%'", hash[:]).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("the table keeps the token in the clear: %d, %v", rows, err)
	}

	signOut := control.Entry{At: t0.Add(2 * time.Hour), Request: "R2", Kind: "sign-out", Decision: control.Allowed}
	if err := s.EndSession(ctx, hash[:], "bob", t0.Add(2*time.Hour), signOut); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionByHash(ctx, hash[:], control.Browser, t0.Add(3*time.Hour)); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("an ended session found %v", err)
	}
	if err := s.EndSession(ctx, hash[:], "bob", t0.Add(4*time.Hour), signOut); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("a session ended twice: %v", err)
	}
	if log := controltest.Audit(t, u, controltest.AuditFilter{Person: sess.Person}); len(log) != 1 || log[0].Kind != "sign-in" {
		t.Fatalf("audit %+v", log)
	}
}

func TestSessionsByAdmin(t *testing.T) { each(t, testSessionsByAdmin) }

// testSessionsByAdmin lists the live sessions and ends one by its id, as
// an admin offboarding a person does.
func testSessionsByAdmin(t *testing.T, s *control.Store, u string) {
	ctx := context.Background()
	mk := func(id, person string, carrier control.Carrier, at time.Time) {
		t.Helper()
		h := sha256.Sum256([]byte("writs_" + id))
		sess := control.Session{ID: id, Carrier: carrier, Person: person, Display: id, Issuer: "sso", Subject: id, Teams: []string{"platform"}, CreatedAt: at, EndsAt: at.Add(12 * time.Hour)}
		if err := s.CreateSession(ctx, sess, h[:], control.Entry{At: at, Request: "R" + id, Kind: "sign-in", Decision: control.Allowed}); err != nil {
			t.Fatal(err)
		}
	}
	mk("A", "gitlab.com/user/1", control.CLI, t0)
	mk("B", "gitlab.com/user/2", control.Browser, t0.Add(time.Minute))
	mk("C", "gitlab.com/user/1", control.CLI, t0.Add(-13*time.Hour))
	got, err := s.Sessions(ctx, "", t0.Add(2*time.Minute))
	if err != nil || len(got) != 2 || got[0].ID != "B" || got[1].ID != "A" || got[1].Carrier != control.CLI {
		t.Fatalf("live sessions %+v, %v", got, err)
	}
	if got, err := s.Sessions(ctx, "gitlab.com/user/1", t0.Add(2*time.Minute)); err != nil || len(got) != 1 || got[0].ID != "A" {
		t.Fatalf("sessions of user 1 %+v, %v", got, err)
	}
	e := control.Entry{At: t0.Add(3 * time.Minute), Request: "RX", Kind: "request", Decision: control.Allowed, Reason: "ended session A"}
	ended, err := s.EndSessionByID(ctx, "A", "admin", t0.Add(3*time.Minute), e)
	if err != nil || ended.ID != "A" || ended.Person != "gitlab.com/user/1" {
		t.Fatalf("ended %+v, %v", ended, err)
	}
	h := sha256.Sum256([]byte("writs_A"))
	if _, err := s.SessionByHash(ctx, h[:], control.CLI, t0.Add(4*time.Minute)); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("an ended session still signs in: %v", err)
	}
	if _, err := s.EndSessionByID(ctx, "A", "admin", t0.Add(5*time.Minute), e); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("a session ended twice: %v", err)
	}
	if _, err := s.EndSessionByID(ctx, "C", "admin", t0.Add(5*time.Minute), e); !errors.Is(err, control.ErrNotFound) {
		t.Fatalf("a session past its end ended: %v", err)
	}
	if log := controltest.Audit(t, u, controltest.AuditFilter{Request: "RX"}); len(log) != 1 {
		t.Fatalf("audit of the end %+v", log)
	}
}
