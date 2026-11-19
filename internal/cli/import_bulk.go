package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/mr-karan/writ/internal/importer"
	"github.com/mr-karan/writ/internal/tofu"
	"github.com/mr-karan/writ/internal/types"
)

const importBulkFormat = "writ.import-bulk/v0"

// Defaults of writ import bulk: the file it writes when --out is unset,
// and its limits.
const (
	DefaultBulkMaxObjects = importer.DefaultMaxObjects
	DefaultBulkMaxBytes   = importer.DefaultMaxBytes
)

// ImportsBulk reports whether the positional arguments of writ import ask
// for a bulk import: bulk alone, as with --type and --id, or bulk and a
// selection file, whose name ends in .json or is - for stdin. Any other
// writ import bulk ID imports one object at an address named bulk.
func ImportsBulk(pos []string) bool {
	switch {
	case len(pos) == 0 || pos[0] != "bulk":
		return false
	case len(pos) == 1:
		return true
	}
	return len(pos) == 2 && (pos[1] == "-" || strings.HasSuffix(pos[1], ".json"))
}

// importBulkReport is `writ import bulk --json`.
type importBulkReport struct {
	Format string `json:"format"`
	OK     bool   `json:"ok"`
	// File is the file written in the stack, "" when nothing was.
	File string `json:"file"`
	// Imports is, in an OpenTofu stack, the imports file written beside
	// File, "" when nothing was written.
	Imports string `json:"imports_file,omitempty"`
	// Imported, Run, and Plan stay false, "", and null: writ import bulk
	// writes no state and plans nothing. The JSON shape keeps them.
	Imported bool             `json:"imported"`
	Run      string           `json:"run"`
	Objects  []jsonBulkObject `json:"objects"`
	Groups   int              `json:"groups"`
	Singles  int              `json:"singles"`
	Linked   int              `json:"linked"`
	Literals int              `json:"literals"`
	// Params never carry their values.
	Params      []jsonDeclaredParam `json:"params"`
	Problems    []jsonImportProblem `json:"problems"`
	Plan        json.RawMessage     `json:"plan"`
	Diagnostics []jsonDiag          `json:"diagnostics"`
}

type jsonBulkObject struct {
	Addr string `json:"addr"`
	Type string `json:"type"`
	// ID is "" when the import id is one of the object's own secrets.
	ID       string `json:"id"`
	SecretID bool   `json:"secret_id"`
	Origin   string `json:"origin"`
}

// ImportBulkOptions are the arguments of `writ import bulk`.
type ImportBulkOptions struct {
	JSON bool
	// Selection is the selection file, "-" for stdin. Without it, Type
	// and IDs select the objects.
	Selection string
	Type      string
	IDs       []string
	// Provider goes with Type: the provider configuration to read through.
	Provider string
	// Out is the file to write in the stack directory.
	Out          string
	MaxObjects   int
	MaxBytes     int
	MaxStaleness *time.Duration
	Params       map[string]string
}

// bulkImport is one run of writ import bulk.
type bulkImport struct {
	o   ImportBulkOptions
	rep importBulkReport
}

// ImportBulk reads many existing objects into an OpenTofu stack: it writes
// the resource blocks that declare them into one new file, and the
// objects it selected into another, for writ import accept.
func (a *App) ImportBulk(ctx context.Context, o ImportBulkOptions) (err error) {
	defer escapeError(&err, o.JSON)
	if !importer.IsTofuStack(a.opts.Dir) {
		return usagef("%s is not an OpenTofu stack: writ import bulk needs .tf files, a %s, and no .writ file; run tofu init first", a.opts.Dir, tofu.LockFile)
	}
	return a.importBulkTofu(ctx, o)
}

// selections reads the selection file, or builds one selection from Type
// and IDs, with typeOf reading each type.
func (a *App) selections(o ImportBulkOptions, typeOf func(string) (types.Name, error)) ([]importer.Selection, error) {
	switch {
	case o.Selection != "" && (o.Type != "" || len(o.IDs) > 0 || o.Provider != ""):
		return nil, usagef("give a selection file or --type and --id, not both")
	case o.Selection != "":
		var r io.Reader = os.Stdin
		if o.Selection != "-" {
			f, err := os.Open(o.Selection)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			r = f
		}
		sels, err := importer.ParseSelectionsOf(r, typeOf)
		if err != nil {
			return nil, usagef("%s: %s", o.Selection, err)
		}
		return sels, nil
	case o.Type == "" || len(o.IDs) == 0:
		return nil, usagef("import bulk needs a selection file, writ import bulk SELECTION.json, or a type and ids, writ import bulk --type TYPE --id ID...")
	}
	typ, err := typeOf(o.Type)
	if err != nil {
		return nil, usagef("--type: %s", err)
	}
	s := importer.Selection{Type: typ, IDs: o.IDs}
	for _, id := range o.IDs {
		if err := importer.CheckID(id); err != nil {
			return nil, usagef("%s", err)
		}
	}
	if o.Provider != "" {
		if s.Provider, err = parseAddr(o.Provider); err != nil {
			return nil, err
		}
	}
	return []importer.Selection{s}, nil
}

// writeNew writes data to a file that must not exist. It creates the file
// exclusively, so a file made after the first check is never replaced; an
// error leaves no partial file behind.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return errors.Join(err, rerr)
		}
	}
	return err
}

func countProblems(res *importer.BulkResult, k importer.ProblemKind) int {
	n := 0
	for _, p := range res.Problems {
		if p.Kind == k {
			n++
		}
	}
	return n
}

func (bi *bulkImport) fill(res *importer.BulkResult) {
	for _, o := range res.Objects {
		bi.rep.Objects = append(bi.rep.Objects, jsonBulkObject{Addr: o.Addr.String(), Type: o.Type.String(), ID: o.ID, SecretID: o.ID == "", Origin: o.Origin})
	}
	for _, p := range res.Params {
		bi.rep.Params = append(bi.rep.Params, jsonDeclaredParam{Name: p.Name, Type: p.Type, Attr: p.Attr})
	}
	for _, p := range res.Problems {
		bi.rep.Problems = append(bi.rep.Problems, jsonImportProblem{Kind: p.Kind.String(), From: p.From, Reason: p.Reason})
	}
	bi.rep.Groups, bi.rep.Singles, bi.rep.Linked, bi.rep.Literals = res.Groups, res.Singles, res.Linked, res.Literals
}

type jsonImportProblem struct {
	Kind   string `json:"kind"`
	From   string `json:"from"`
	Reason string `json:"reason"`
}

// jsonDeclaredParam is one param the printed code declares for a sensitive
// value. Its value is never printed.
type jsonDeclaredParam struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Attr string `json:"attr"`
}
