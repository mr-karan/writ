package server

import (
	"net/http"
	"testing"
)

// Only MountUI adds a route under /ui/, in any form of pattern, and it
// adds the control UI's table once.
func TestOnlyMountUIServesUI(t *testing.T) {
	panics := func(f func()) (p bool) {
		defer func() { p = recover() != nil }()
		f()
		return false
	}
	h := http.HandlerFunc(http.NotFound)
	for _, c := range []struct {
		pattern string
		fromUI  bool
		panics  bool
	}{
		{"/ui/", false, true},
		{"/ui", false, true},
		{"GET /ui/plans", false, true},
		{"GET\t/ui/plans/{id}", false, true},
		{"POST example.com/ui/logout", false, true},
		{"example.com/ui/", false, true},
		{"/%75i/plans", false, true},
		{"GET /%75I/plans", false, false},
		{"example.com/", false, true},
		{"GET example.com/{path...}", false, true},
		{"GET example.com/v1/plans", false, true},
		{"/{seg}", false, true},
		{"GET /{seg}/plans", false, true},
		{"GET /{$}", false, true},
		{"GET /ui/plans", true, false},
		{"GET /{$}", true, false},
		{"GET /v1/ui/x", false, false},
		{"GET /uix", false, false},
		{"/", false, false},
	} {
		t.Run(c.pattern, func(t *testing.T) {
			m := &apiMux{mux: http.NewServeMux()}
			if got := panics(func() { m.handle(c.pattern, h, c.fromUI) }); got != c.panics {
				t.Errorf("handle(%q, fromUI %v) panicked: %v, want %v", c.pattern, c.fromUI, got, c.panics)
			}
		})
	}

	s := &Server{api: &apiMux{mux: http.NewServeMux()}}
	s.MountUI([]UIRoute{{"GET /ui/one", h}}, nil)
	if !panics(func() { s.MountUI([]UIRoute{{"GET /ui/two", h}}, nil) }) {
		t.Error("a second MountUI added a table")
	}
}
