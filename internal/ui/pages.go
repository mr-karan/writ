package ui

import (
	"net/http"

	"github.com/mr-karan/writ/internal/server"
)

func (f *fetch) badRequest(msg string) {
	f.u.errorPage(f.w, f.r, f.sess, http.StatusBadRequest, "bad request", []server.Diagnostic{{Severity: "error", Class: "usage", Message: msg}}, nil)
}

// envNames lists the environments the signed-in person may read, from the
// overview document the sidebar reads too, and reports whether it came.
func (f *fetch) envNames() ([]string, bool) {
	a := f.activity(true)
	if a == nil {
		return nil, false
	}
	return f.envsOf(a), true
}

// listEnv is the environment a list page shows, env or else the first the
// signed-in person may read, and the environments of its tabs. With no
// env and the overview failed, it renders the overview's refusal and
// reports false.
func (f *fetch) listEnv(env string) (string, []string, bool) {
	envs, read := f.envNames()
	if env == "" && !read && f.failed("/v1/overview") {
		return "", nil, false
	}
	if env == "" && len(envs) > 0 {
		env = envs[0]
	}
	return env, envs, true
}

// failed renders the refusal of target's read and reports true, unless
// target came or answered 404, which the page reads as none.
func (f *fetch) failed(target string) bool {
	status, body := f.call(target)
	if status == http.StatusOK || status == http.StatusNotFound {
		return false
	}
	f.refused(status, body)
	return true
}
