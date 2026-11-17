package server

import (
	"fmt"
	"net/netip"
	"time"

	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/control"
	"github.com/mr-karan/writ/internal/oidc"
)

// Config is writ serve's config file, an HCL file, parsed into typed
// values. It holds no secret: keys, tokens, and passwords are files it
// names.
type Config struct {
	// Dev is writ serve --dev: one user with the token Options.DevToken,
	// on loopback, with every environment served. DevConfig makes it.
	Dev bool
	// Text is the file as read, which control.db keeps with each start.
	Text string
	// URL is writ serve's own URL, and the only audience it accepts.
	URL    string
	Listen string
	// TLS is nil when writ serve listens on a loopback address behind a
	// proxy that ends TLS.
	TLS *TLSFiles
	// Database is the Postgres URL of the control database, which also
	// holds the state of every stack@env writ serve plans. Under --dev it
	// may be the path of a SQLite file.
	Database string
	// Data is a directory for work files that only writ reads.
	Data    string
	PlanTTL time.Duration
	// SessionTTL is how long a person's session lasts. There is no refresh.
	SessionTTL time.Duration
	// UI serves the control UI under /ui/. It needs a people issuer.
	UI     bool
	Limits Limits
	// TrustedProxies are the peers whose X-Forwarded-For names the
	// client.
	TrustedProxies []netip.Prefix
	Retention      control.Retention
	Issuers        []Issuer
	Policy         authz.Policy
	Envs           map[string]Env
	// AWS is how writ serve reaches STS. It is nil when no environment
	// names a role.
	AWS *AWS
	// ProviderUIDs are the uids providers run as, one for each running
	// plan. Nil runs providers as writ's own user, which a config with a
	// role refuses.
	ProviderUIDs *UIDRange
	// Forges are where repositories are read from, by name.
	Forges []Forge
	// Repos pins the repositories when PinRepos is set: the rows follow
	// the list, and POST /v1/repos is refused.
	Repos    []RepoSpec
	PinRepos bool
	// PollEvery is how often each repository's default branch is polled.
	PollEvery time.Duration
	// Tofu is how writ serve runs OpenTofu for the stacks of .tf files.
	// Nil refuses them.
	Tofu *Tofu
	// Policies judge the plans of OpenTofu stacks. Nil judges none.
	Policies *Policies
	// DriftEvery is how often writ serve plans each OpenTofu stack@env it
	// keeps state for, to find drift. 0 makes no drift check.
	DriftEvery time.Duration
}

// minDriftEvery is the shortest drift.every: each check reads every
// resource of the stack through its providers, with the plan role.
const minDriftEvery = time.Minute

// Tofu is the OpenTofu that writ serve runs, and the directories its runs
// share.
type Tofu struct {
	// Binary is the tofu executable, which must be OpenTofu 1.13.9. A name
	// with no slash is looked up on PATH.
	Binary string
	// PluginCache holds the providers. writ fills it as its own user, and
	// a run as a provider uid only reads it.
	PluginCache string
	// ScratchDir holds each run's scratch directory.
	ScratchDir string
	// Git, when set, is the git executable that each run's filesystem
	// view holds, for a module source that needs git. A name with no slash
	// is looked up on PATH.
	Git string
}

// Forge is a forge the control plane reads repositories from, with a
// read-only token in a file. The config holds no token.
type Forge struct {
	Name string
	Kind control.RepoKind
	URL  string
	// ReadTokenFile holds the read token: a group or project access token
	// with read_api and the Reporter role, or a bot user's personal access
	// token on GitLab.com Free.
	ReadTokenFile string
	// CAFile names extra roots for the forge's certificate.
	CAFile string
}

// RepoSpec is a repository as a request or the config names it.
type RepoSpec struct {
	Name string
	// Forge is a forge's name, or the host of its URL. Under --dev, dir
	// names a local Git repository, and Project is its absolute path.
	Forge   string
	Project string
	// Path is the directory in the project that holds the stacks.
	Path string
	// Prefix is put before each stack's directory to make its name.
	Prefix string
}

// AWS is how writ serve assumes the environments' roles.
type AWS struct {
	Region string
	// STSEndpoint replaces the regional endpoint. "" uses the SDK's.
	STSEndpoint string
	// Session is how long each set of credentials lasts. The endpoint
	// assumes the role again before they expire.
	Session time.Duration
}

// DefaultSession is AWS's limit for a role session made from another role
// session, as a control plane on EC2 makes.
const DefaultSession = time.Hour

// UIDRange is a range of uids with no other user on the host. Each uid's
// group has the same number.
type UIDRange struct {
	First, Last uint32
}

func (r UIDRange) String() string { return fmt.Sprintf("%d-%d", r.First, r.Last) }

// Count is how many uids the range holds.
func (r UIDRange) Count() int { return int(r.Last-r.First) + 1 }

// Contains reports whether uid is in the range.
func (r UIDRange) Contains(uid uint32) bool { return uid >= r.First && uid <= r.Last }

// TLSFiles are the certificate chain and key writ serve answers with.
type TLSFiles struct{ Cert, Key string }

// Limits bound what one principal and the whole server do at once.
type Limits struct {
	RequestsPerSecond int
	// RequestsPerAddress bounds the requests a second from one client
	// address, counted before the token is checked.
	RequestsPerAddress int
	PlansPerPrincipal  int
	// Plans bounds the plans running on the server at once.
	Plans int
	// Applies bounds the applies running on the server at once.
	Applies int
	// Body bounds a request body after gzip decompression.
	Body int64
	// PlanTimeout bounds a plan from its start to its end, the wait for a
	// provider uid included.
	PlanTimeout time.Duration
	// ApplyStartTimeout bounds what an apply waits for before its first
	// change: the saved plan, the role's credentials, and a provider uid.
	ApplyStartTimeout time.Duration
	// ApplyTimeout is when a running apply stops starting changes. The
	// changes already running finish.
	ApplyTimeout time.Duration
}

// Issuer is one issuer writ serve trusts.
type Issuer struct {
	Name string
	Kind oidc.Kind
	URL  string
	// Identity names the forge or identity provider whose user ids the
	// issuer's tokens carry. A person is Identity/user/ID, so two issuers
	// of one GitLab that share it name one person whatever their URLs.
	Identity string
	// CAFile names extra roots for the issuer's certificate, for an
	// instance with a private CA. "" trusts the system roots.
	CAFile string
	// ClientID and ClientSecretFile are writ serve's OAuth client at a
	// people issuer. The file holds the secret, which never enters the
	// config.
	ClientID         string
	ClientSecretFile string
	// GroupsFromUserinfo reads a person's groups from the userinfo
	// endpoint at sign-in instead of the ID token.
	GroupsFromUserinfo bool
}

// identity is the identity of the issuer the config names name, "" for
// an issuer it does not define.
func (c *Config) identity(name string) string {
	for _, is := range c.Issuers {
		if is.Name == name {
			return is.Identity
		}
	}
	return ""
}

// Env is one environment writ serve plans in.
type Env struct {
	Name string
	// Key is the environment's state key file. It never leaves the host.
	Key string
	// PlanRole is the role a plan's providers get credentials for. ""
	// gives them none.
	PlanRole string
	// ApplyRole is the role an apply's providers get credentials for. ""
	// gives them none.
	ApplyRole string
	// S3Endpoints are the custom endpoints a stack's S3 backend may name,
	// each an exact URL. A custom endpoint gets the requests the role
	// signs, so a stack may name only one the config lists.
	S3Endpoints []string
	// ProviderEnv maps each variable a provider gets to the file that
	// holds its value.
	ProviderEnv map[string]string
}

// Defaults the design sets.
const (
	DefaultPlanTTL            = 24 * time.Hour
	DefaultSessionTTL         = 12 * time.Hour
	DefaultRequestsPerSecond  = 10
	DefaultRequestsPerAddress = 20
	DefaultPlansPerPrincipal  = 4
	DefaultPlans              = 8
	DefaultApplies            = 4
	DefaultBody               = 16 << 20
	DefaultPlanTimeout        = 30 * time.Minute
	DefaultApplyStartTimeout  = 10 * time.Minute
	DefaultApplyTimeout       = 4 * time.Hour
	DefaultPollEvery          = time.Minute
)
