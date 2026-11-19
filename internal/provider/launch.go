package provider

import (
	"errors"
	"os"

	"github.com/mr-karan/writ/internal/runas"
)

// Launch says how provider processes start. The zero value starts them as
// writ's own user with writ's environment, filtered by providerEnv, as
// writ plan on a laptop does.
type Launch struct {
	nomadPlanResponse bool
	// OwnEnv makes Env the provider's whole environment, apart from the
	// plugin settings and HOME and TMPDIR, which name the process's own
	// directory. Nothing then comes from writ's environment. writ serve
	// sets it, so a provider sees one run's credentials and no others.
	OwnEnv bool
	Env    []string
	// User, when set, runs each provider as that user. Its directory,
	// which holds the plugin socket, belongs to that user.
	User *User
}

// User is an OS user and group with no name. writ serve gives each run its
// own from the config's provider_uids.
type User = runas.User

// ErrIsolationUnsupported is returned where providers cannot run as another
// user.
var ErrIsolationUnsupported = errors.New("running providers as another user needs Linux")

// isolatedSocketRoot is where an isolated provider's directory goes: a
// directory every user may create in. Under systemd's PrivateTmp it is the
// unit's own.
const isolatedSocketRoot = "/tmp"

// launchEnv is the environment a provider starts with, before the plugin
// settings.
func (l Launch) launchEnv(dir string) []string {
	if !l.OwnEnv {
		return providerEnv(os.Environ())
	}
	env := append([]string(nil), l.Env...)
	return append(env, "HOME="+dir, "TMPDIR="+dir)
}
