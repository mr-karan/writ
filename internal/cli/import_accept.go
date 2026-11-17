package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/mask"
	"github.com/mr-karan/writ/internal/server"
)

// AcceptsPlan reports whether the positional arguments of writ import ask
// to accept the plan of an OpenTofu stack: accept and a plan, the plan file
// that tofu plan -out saved, a JSON document, or - for JSON on stdin.
func AcceptsPlan(pos []string) bool {
	return len(pos) == 2 && pos[0] == "accept"
}

// importAcceptFormat is writ import accept --json with a plan.
const importAcceptFormat = "writ.import-accept/v0"

// maxAcceptPlan and maxAcceptImports bound the files writ import accept
// reads, and maxAcceptPlan the output of tofu show -json too.
const (
	maxAcceptPlan    = 64 << 20
	maxAcceptImports = 16 << 20
)

// tofuShowTimeout bounds tofu show -json of a saved plan, which starts
// each provider of the plan to read its schema.
const tofuShowTimeout = 5 * time.Minute

// zipMagic starts a plan file that tofu plan -out saves, a zip archive. No
// JSON document starts with it.
var zipMagic = []byte("PK\x03\x04")

// readBounded reads the file at path, refusing one larger than limit.
func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, usagef("%s is larger than %d bytes", path, limit)
	}
	return raw, nil
}

// ImportAcceptPlanOptions are the arguments of writ import accept PLAN.
type ImportAcceptPlanOptions struct {
	JSON bool
	// Plan is the plan file that tofu plan -out saved for the stack after
	// the import, or a file, "-" for stdin, that holds a JSON document of
	// that plan: the masked plan document, writ plan --remote --json's
	// output, or tofu show -json's. A JSON document is accepted only for
	// an import that plans no update, since nothing proves which plan it
	// shows.
	Plan string
	// Imports is the file in which writ import bulk recorded each object
	// it selected, the address and the import id of each.
	Imports string
}

type importAcceptReport struct {
	Format      string              `json:"format"`
	OK          bool                `json:"ok"`
	Objects     int                 `json:"objects"`
	Imports     int                 `json:"imports"`
	Refusals    []jsonAcceptRefusal `json:"refusals"`
	Settings    []jsonAcceptSetting `json:"settings"`
	Diagnostics []jsonDiag          `json:"diagnostics"`
}

// jsonAcceptSetting is a provider-only setting that an accepted plan sets
// in place on an object it imports.
type jsonAcceptSetting struct {
	Address   string `json:"address"`
	Attribute string `json:"attribute"`
	Value     any    `json:"value"`
	Provider  string `json:"provider"`
	Version   string `json:"version"`
}

type jsonAcceptRefusal struct {
	Address string `json:"address"`
	Reason  string `json:"reason"`
}

// ImportAcceptPlan accepts the plan of an OpenTofu stack after writ import
// bulk: it passes when the plan imports exactly the objects the import
// selected, each at its address by its import id, and changes nothing but
// the provider-only settings it prints.
func (a *App) ImportAcceptPlan(ctx context.Context, o ImportAcceptPlanOptions) (err error) {
	defer escapeError(&err, o.JSON)
	if o.Imports == "" {
		return usagef("writ import accept PLAN needs --imports FILE, the file such as %s that writ import bulk wrote beside the code", ImportsFile(DefaultTofuBulkFile))
	}
	importsText, err := readBounded(o.Imports, maxAcceptImports)
	if err != nil {
		return err
	}
	want, err := importer.ParseImports(importsText)
	if err != nil {
		return usagef("%s: %s", o.Imports, err)
	}
	var raw []byte
	if o.Plan == "-" {
		if raw, err = io.ReadAll(io.LimitReader(os.Stdin, maxAcceptPlan+1)); err != nil {
			return err
		}
		if len(raw) > maxAcceptPlan {
			return usagef("stdin holds more than %d bytes", maxAcceptPlan)
		}
	} else if raw, err = readBounded(o.Plan, maxAcceptPlan); err != nil {
		return err
	}
	var acc importer.Acceptance
	switch {
	case !bytes.HasPrefix(raw, zipMagic):
		acc, err = acceptJSON(raw, want, importer.QuoteIDs)
	case o.Plan == "-":
		return usagef("stdin holds a saved plan file; pass its path, so tofu show -json can read it")
	default:
		var shown []byte
		if shown, err = tofuShow(ctx, a.acceptDir(o.Imports), o.Plan); err != nil {
			return err
		}
		acc, err = importer.AcceptTofuPlan(shown, want)
	}
	if err != nil {
		return usagef("%s: %s", o.Plan, err)
	}
	return a.printAccept(o.JSON, len(want), acc)
}

// printAccept prints acc, the judgment of a plan after an import that
// selected that many objects, and fails when acc holds a refusal.
func (a *App) printAccept(asJSON bool, objects int, acc importer.Acceptance) error {
	rep := importAcceptReport{Format: importAcceptFormat, OK: len(acc.Refusals) == 0, Objects: objects, Imports: acc.Imports, Refusals: []jsonAcceptRefusal{}, Settings: []jsonAcceptSetting{}, Diagnostics: []jsonDiag{}}
	for _, f := range acc.Refusals {
		rep.Refusals = append(rep.Refusals, jsonAcceptRefusal{Address: f.Address, Reason: f.Reason})
	}
	if rep.OK {
		for _, s := range acc.Settings {
			rep.Settings = append(rep.Settings, jsonAcceptSetting{Address: s.Address, Attribute: s.Attribute, Value: s.Value, Provider: s.Provider, Version: s.Version})
		}
	}
	if asJSON {
		if err := writeJSON(a.opts.Stdout, rep); err != nil {
			return err
		}
	} else {
		w := a.humanOutput()
		for _, f := range rep.Refusals {
			if f.Address != "" {
				fmt.Fprintf(w, "! %s: %s\n", escapeHuman(f.Address, false), f.Reason)
			} else {
				fmt.Fprintf(w, "! %s\n", f.Reason)
			}
		}
		for _, s := range rep.Settings {
			value, err := json.Marshal(s.Value)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "~ %s: sets %s = %s, a setting that %s %s keeps and the cloud does not return\n", escapeHuman(s.Address, false), escapeHuman(s.Attribute, false), escapeHuman(string(value), false), s.Provider, s.Version)
		}
		switch {
		case !rep.OK:
			fmt.Fprintln(w, "Not accepted: edit the imported code until the plan only imports.")
		case len(rep.Settings) > 0:
			fmt.Fprintf(w, "Accepted: the plan imports %s, sets %s that the cloud does not return, and changes nothing else.\n", plural(acc.Imports, "object"), plural(len(rep.Settings), "provider-only setting"))
		default:
			fmt.Fprintf(w, "Accepted: the plan imports %s and changes nothing else.\n", plural(acc.Imports, "object"))
		}
	}
	if !rep.OK {
		return ErrFailed
	}
	return nil
}

// acceptDir is the stack directory in which tofu show reads a saved plan:
// --dir when it names another directory than ".", else the directory of
// the imports file, which writ import bulk writes beside the code.
func (a *App) acceptDir(imports string) string {
	if filepath.Clean(a.opts.Dir) != "." {
		return a.opts.Dir
	}
	return filepath.Dir(imports)
}

// tofuShow runs tofu show -json of plan, a saved plan file, in dir, the
// stack's directory, whose .terraform holds the providers whose schemas
// tofu reads, and returns what it prints. tofu comes from PATH. Its stderr
// is not printed, because the mask has not read it.
func tofuShow(ctx context.Context, dir, plan string) ([]byte, error) {
	abs, err := filepath.Abs(plan)
	if err != nil {
		return nil, err
	}
	bin, err := exec.LookPath("tofu")
	if err != nil {
		return nil, fmt.Errorf("writ import accept reads a saved plan with tofu show -json: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, tofuShowTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "show", "-json", abs)
	cmd.Dir = dir
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(out, maxAcceptPlan+1))
	over := len(raw) > maxAcceptPlan
	if over || readErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	switch {
	case over:
		return nil, fmt.Errorf("tofu show -json printed more than %d bytes", maxAcceptPlan)
	case readErr != nil:
		return nil, readErr
	case waitErr != nil:
		return nil, fmt.Errorf("tofu show -json %s in %s: %w; run it there to see why", plan, dir, waitErr)
	}
	return raw, nil
}

// acceptJSON judges the JSON plan in raw after an import that selected
// want: a writ.remote-plan document's masked plan, a masked plan document,
// or the output of tofu show -json, which the importer masks first. Which
// of the last two it is decides only how it is read: neither proves what
// an update changes, so the importer refuses every import update of both.
// q says whether a refusal may quote an import id.
func acceptJSON(raw []byte, want []importer.Import, q importer.Quoting) (importer.Acceptance, error) {
	var head struct {
		Format        string          `json:"format"`
		Document      json.RawMessage `json:"document"`
		PlannedValues json.RawMessage `json:"planned_values"`
		Variables     json.RawMessage `json:"variables"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return importer.Acceptance{}, errors.New("neither a plan file that tofu plan -out saved nor a JSON plan")
	}
	if head.Format == server.FormatRemotePlan {
		if len(head.Document) == 0 || string(head.Document) == "null" {
			return importer.Acceptance{}, errors.New("the remote plan has no plan document yet")
		}
		raw, head.Format = head.Document, ""
		head.PlannedValues, head.Variables = nil, nil
		if err := json.Unmarshal(raw, &head); err != nil {
			return importer.Acceptance{}, errors.New("the remote plan's document is not a JSON plan")
		}
	}
	if head.Format != "" {
		return importer.Acceptance{}, errors.New("the document has a format field, so it is not the plan of an OpenTofu stack")
	}
	if len(head.PlannedValues) > 0 || bytes.HasPrefix(bytes.TrimSpace(head.Variables), []byte("{")) {
		return importer.AcceptTofuJSON(raw, want, q)
	}
	var doc mask.Plan
	if err := json.Unmarshal(raw, &doc); err != nil {
		return importer.Acceptance{}, errors.New("not a masked plan document")
	}
	if doc.FormatVersion == "" {
		return importer.Acceptance{}, errors.New("not a masked plan document: it has no format_version")
	}
	return importer.AcceptPlan(&doc, want, q)
}
