package server

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/mr-karan/writ/internal/authz"
	"github.com/mr-karan/writ/internal/confine"
	"github.com/mr-karan/writ/internal/notify"
)

// maxNotifyFileBytes bounds a url_file or a secret_file.
const maxNotifyFileBytes = 4096

// notifiers decodes the config's notify blocks.
func (d *decoder) notifiers(bs []*hclsyntax.Block, cfg *Config) {
	for _, blk := range bs {
		name := blk.Labels[0]
		if !nameRE.MatchString(name) {
			d.errorf(blk.LabelRanges[0], "notify name %q must be lowercase letters, digits, _ and -", name)
		}
		what := "notify " + name
		r := d.attrs(blk, what, "kind", "url_file", "secret_file", "events", "envs", "stacks", "drift_mode")
		n := Notifier{Name: name, DriftMode: notify.DriftOnChange}
		if f, ok := r["kind"]; !ok {
			d.errorf(blk, "%s needs kind, \"slack\" or \"webhook\"", what)
		} else if raw := d.str(f.value, what+" kind"); raw != "" {
			kind, err := notify.ParseKind(raw)
			if err != nil {
				d.errorf(f.value, "%s: %v", what, err)
			}
			n.Kind = kind
		}
		if f, ok := r["url_file"]; ok {
			n.URLFile = d.path(f.value, what+" url_file")
		} else {
			d.errorf(blk, "%s needs url_file, the file that holds the webhook URL", what)
		}
		if f, ok := r["secret_file"]; ok {
			if n.Kind == notify.KindSlack {
				d.errorf(f.key, "%s: secret_file goes with kind webhook; Slack does not check a signature", what)
			} else {
				n.SecretFile = d.path(f.value, what+" secret_file")
			}
		}
		if f, ok := r["events"]; !ok {
			d.errorf(blk, "%s needs events, a list of: %s", what, eventNames())
		} else {
			raw := d.strs(f.value, what+" events")
			if len(raw) == 0 {
				d.errorf(f.value, "%s: events needs at least one of: %s", what, eventNames())
			}
			for _, s := range raw {
				ev, err := notify.ParseEvent(s)
				switch {
				case err != nil:
					d.errorf(f.value, "%s: %v", what, err)
				case slices.Contains(n.Events, ev):
					d.errorf(f.value, "%s: events lists %s twice", what, ev)
				default:
					n.Events = append(n.Events, ev)
				}
			}
		}
		if f, ok := r["envs"]; ok {
			n.Envs = d.strs(f.value, what+" envs")
			if len(n.Envs) == 0 {
				d.errorf(f.value, "%s: envs needs at least one env; leave it out for every env", what)
			}
			for _, e := range n.Envs {
				if _, ok := cfg.Envs[e]; !ok && e != "*" {
					d.errorf(f.value, "%s names env %s, which no env block defines", what, e)
				}
			}
		}
		if f, ok := r["stacks"]; ok {
			n.Stacks = d.strs(f.value, what+" stacks")
			if len(n.Stacks) == 0 {
				d.errorf(f.value, "%s: stacks needs at least one pattern; leave it out for every stack", what)
			}
			for _, pat := range n.Stacks {
				if err := authz.ParsePattern(pat); err != nil {
					d.errorf(f.value, "%s: the stack pattern %q %v", what, pat, err)
				}
			}
		}
		if f, ok := r["drift_mode"]; ok {
			if raw := d.str(f.value, what+" drift_mode"); raw != "" {
				mode, err := notify.ParseDriftMode(raw)
				if err != nil {
					d.errorf(f.value, "%s: %v", what, err)
				}
				n.DriftMode = mode
			}
			if !slices.Contains(n.Events, notify.EventDrift) {
				d.errorf(f.key, "%s sets drift_mode, and its events do not list drift", what)
			}
		}
		cfg.Notifiers = append(cfg.Notifiers, n)
	}
	slices.SortFunc(cfg.Notifiers, func(a, b Notifier) int { return strings.Compare(a.Name, b.Name) })
}

func eventNames() string {
	names := make([]string, len(notify.Events))
	for i, ev := range notify.Events {
		names[i] = string(ev)
	}
	return strings.Join(names, ", ")
}

// matches reports whether the notifier hears of an event on stack@env: its
// envs, as a grant's, list env or "*", and one of its stack patterns
// matches stack. Leaving either out means all.
func (n Notifier) matches(env, stack string) bool {
	if len(n.Envs) > 0 && !slices.Contains(n.Envs, "*") && !slices.Contains(n.Envs, env) {
		return false
	}
	return len(n.Stacks) == 0 || slices.ContainsFunc(n.Stacks, func(p string) bool { return authz.MatchStack(p, stack) })
}

// LoadNotifiers reads each notifier's url_file and secret_file and checks
// the URL. An error names the notifier and the file, never what the file
// holds.
func LoadNotifiers(cfg *Config) ([]notify.Target, error) {
	var fs confine.Files
	var out []notify.Target
	for _, n := range cfg.Notifiers {
		raw, err := fs.ReadFileMax(n.URLFile, maxNotifyFileBytes)
		if err != nil {
			return nil, fmt.Errorf("notify %s: read url_file: %w", n.Name, err)
		}
		u := strings.TrimSpace(string(raw))
		if err := notify.CheckURL(u); err != nil {
			return nil, fmt.Errorf("notify %s: the URL in url_file %s: %w", n.Name, n.URLFile, err)
		}
		var secret []byte
		if n.SecretFile != "" {
			raw, err := fs.ReadFileMax(n.SecretFile, maxNotifyFileBytes)
			if err != nil {
				return nil, fmt.Errorf("notify %s: read secret_file: %w", n.Name, err)
			}
			if secret = []byte(strings.TrimSpace(string(raw))); len(secret) == 0 {
				return nil, fmt.Errorf("notify %s: secret_file %s is empty", n.Name, n.SecretFile)
			}
		}
		out = append(out, notify.Target{Name: n.Name, Kind: n.Kind, URL: u, Secret: secret, Events: slices.Clone(n.Events), DriftMode: n.DriftMode, Match: n.matches})
	}
	return out, nil
}

// NotifyTest is how a test message to one notifier went.
type NotifyTest struct {
	Name string
	Err  error
}

// TestNotifiers sends one test message to each notifier, with no retry,
// for writ serve --check --notify-test.
func TestNotifiers(ctx context.Context, cfg *Config, now time.Time) ([]NotifyTest, error) {
	targets, err := LoadNotifiers(cfg)
	if err != nil {
		return nil, err
	}
	link := ""
	if cfg.UI {
		link = cfg.URL + "/ui/settings"
	}
	var out []NotifyTest
	for _, t := range targets {
		ev := notify.Event{ID: newID(), Event: notify.EventTest, At: now, Server: cfg.URL, Status: "test",
			Summary:   "a test message: notify " + t.Name + " reaches this channel",
			StartedBy: notify.Actor{Kind: startedBySystem, Display: "writ serve --check --notify-test"}, URL: link}
		out = append(out, NotifyTest{Name: t.Name, Err: notify.SendTest(ctx, t, ev, notify.DefaultTimeout)})
	}
	return out, nil
}
