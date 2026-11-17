package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/mr-karan/writ/internal/addr"
	"github.com/mr-karan/writ/internal/diag"
	"github.com/mr-karan/writ/internal/importer"
)

// DefaultTofuBulkFile is the file writ import bulk writes in an OpenTofu
// stack when --out is unset.
const DefaultTofuBulkFile = "imported.tf"

// ImportsFile is the file beside the code file out in which writ import
// bulk records each object it selected in an OpenTofu stack, for writ
// import accept. Its name does not end in .tf.json, so tofu does not read
// it as code.
func ImportsFile(out string) string { return strings.TrimSuffix(out, ".tf") + ".imports.json" }

// importBulkTofu is writ import bulk in an OpenTofu stack. It reads the
// objects through the providers that .terraform.lock.hcl pins and writes
// resource blocks and import blocks into one new file. It writes no state
// and plans nothing: tofu plan reads the objects again and adopts them.
func (a *App) importBulkTofu(ctx context.Context, o ImportBulkOptions) error {
	if o.Out == "" {
		o.Out = DefaultTofuBulkFile
	}
	if o.Out != filepath.Base(o.Out) || !strings.HasSuffix(o.Out, ".tf") || strings.HasPrefix(o.Out, ".") {
		return usagef("--out %q: want the name of a new .tf file in the stack directory, such as %s", o.Out, DefaultTofuBulkFile)
	}
	if base := strings.TrimSuffix(o.Out, ".tf"); base == "override" || strings.HasSuffix(base, "_override") {
		return usagef("--out %q: tofu reads %s as an override of the resources other files declare, not as new ones; pick another --out", o.Out, o.Out)
	}
	switch {
	case o.MaxObjects < 0 || o.MaxBytes < 0:
		return usagef("--max-objects and --max-bytes take a positive number")
	case o.MaxStaleness != nil:
		return usagef("--max-staleness is for a Writ stack; in an OpenTofu stack writ import plans nothing")
	case len(o.Params) > 0:
		return usagef("--param is for a Writ stack; an OpenTofu stack takes its variables from tofu")
	}
	importsName := ImportsFile(o.Out)
	path, importsPath := filepath.Join(a.opts.Dir, o.Out), filepath.Join(a.opts.Dir, importsName)
	for _, name := range []string{o.Out, importsName} {
		if _, err := os.Lstat(filepath.Join(a.opts.Dir, name)); err == nil {
			return usagef("%s exists; writ import bulk writes a new file, so pick another --out", name)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	// OpenTofu 1.13.9 ignores X.tf when X.tofu exists, and reads X.tf.json
	// and X.tofu.json beside X.tf.
	base := strings.TrimSuffix(o.Out, ".tf")
	for _, name := range []string{base + ".tofu", base + ".tf.json", base + ".tofu.json"} {
		if _, err := os.Lstat(filepath.Join(a.opts.Dir, name)); err == nil {
			if name == base+".tofu" {
				return usagef("%s exists, and tofu ignores %s beside it; pick another --out", name, o.Out)
			}
			return usagef("%s exists, and tofu reads it with %s, where one declaration of both would clash; pick another --out", name, o.Out)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	dirs, err := a.dirs()
	if err != nil {
		return err
	}
	st, err := importer.OpenTofuStack(ctx, a.opts.Dir, importer.TofuOptions{Dirs: dirs, Log: a.opts.Log, Launch: a.opts.ProviderLaunch})
	if err != nil {
		return err
	}
	defer func() {
		if err := st.Close(context.WithoutCancel(ctx)); err != nil {
			a.opts.Log.Warn("stop providers", "err", err)
		}
	}()
	sels, err := a.selections(o, st.Type)
	if err != nil {
		return err
	}
	bi := &bulkImport{o: o, rep: importBulkReport{
		Format: importBulkFormat, Objects: []jsonBulkObject{}, Params: []jsonDeclaredParam{}, Problems: []jsonImportProblem{},
	}}
	var diags diag.List
	if err := st.Prepare(sels); err != nil {
		diags.Errorf(diag.Usage, diag.Span{}, "%s; nothing was read", err)
		return a.tofuBulkDone(bi, diags)
	}
	res, err := importer.Bulk(ctx, importer.BulkOptions{
		Selections: sels, Reader: st, Lister: st, Secrets: st.Secrets(), Taken: st.Taken(),
		MaxObjects: o.MaxObjects, MaxBytes: o.MaxBytes, File: o.Out, Parallelism: 10,
	})
	if limit, ok := errors.AsType[*importer.LimitError](err); ok {
		diags.Errorf(diag.Usage, diag.Span{}, "%s; nothing was written", limit)
		return a.tofuBulkDone(bi, diags)
	}
	if dup, ok := errors.AsType[*importer.DuplicateError](err); ok {
		diags.Errorf(diag.Usage, diag.Span{}, "%s; nothing was written", dup)
		return a.tofuBulkDone(bi, diags)
	}
	if err != nil {
		return err
	}
	bi.fill(res)
	for i, obj := range res.Objects {
		bi.rep.Objects[i].Type = obj.Schema.Wire
	}
	switch {
	case res.Failed():
		diags.Errorf(diag.Provider, diag.Span{}, "%s could not be imported, so nothing was written", plural(countProblems(res, importer.NotImported), "object"))
		return a.tofuBulkDone(bi, diags)
	case len(res.Objects) == 0:
		diags.Errorf(diag.Usage, diag.Span{}, "the selection names no object to import, so nothing was written")
		return a.tofuBulkDone(bi, diags)
	}
	imports, err := importer.EncodeImports(res.Imports)
	if err != nil {
		return err
	}
	if err := writeNew(importsPath, imports); errors.Is(err, fs.ErrExist) {
		diags.Errorf(diag.Usage, diag.Span{}, "%s appeared while writ import bulk read the objects, so nothing was written; pick another --out", importsName)
		return a.tofuBulkDone(bi, diags)
	} else if err != nil {
		return err
	}
	if err := writeNew(path, res.Code); err != nil {
		if rerr := os.Remove(importsPath); rerr != nil {
			return errors.Join(err, rerr)
		}
		if errors.Is(err, fs.ErrExist) {
			diags.Errorf(diag.Usage, diag.Span{}, "%s appeared while writ import bulk read the objects, so nothing was written; pick another --out", o.Out)
			return a.tofuBulkDone(bi, diags)
		}
		return err
	}
	bi.rep.File, bi.rep.Imports = o.Out, importsName
	return a.tofuBulkDone(bi, diags)
}

// tofuBulkDone prints the report of an import into an OpenTofu stack, and
// fails when diags hold an error.
func (a *App) tofuBulkDone(bi *bulkImport, diags diag.List) error {
	rep := &bi.rep
	rep.OK, rep.Diagnostics = !diags.HasErrors(), toDiags(diags)
	if bi.o.JSON {
		if err := writeJSON(a.opts.Stdout, rep); err != nil {
			return err
		}
	} else {
		w := a.humanOutput()
		printDiags(w, diags)
		if rep.File != "" {
			tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
			for _, o := range rep.Objects {
				fmt.Fprintf(tw, "+ %s\t%s\t%s\n", o.Addr, o.Type, addr.Quote(o.ID))
			}
			tw.Flush()
		}
		for _, p := range rep.Problems {
			fmt.Fprintf(w, "! %s  %s: %s\n", p.From, p.Kind, p.Reason)
		}
		for _, p := range rep.Params {
			fmt.Fprintf(w, "variable %s: %s  from %s; set it with TF_VAR_%s\n", p.Name, p.Type, p.Attr, p.Name)
		}
		if rep.File != "" {
			n := len(rep.Objects)
			fmt.Fprintf(w, "wrote %s: %s with for_each, %s, %s in place of literals, %s left as literals\n",
				rep.File, plural(rep.Groups, "resource"), plural(rep.Singles, "single resource"), plural(rep.Linked, "reference"), plural(rep.Literals, "typed reference"))
			fmt.Fprintf(w, "wrote %s: the objects that tofu plan should import\n", rep.Imports)
			fmt.Fprintf(w, "Read %s and wrote no state. tofu plan should show %d to import, 0 to add, 0 to change, 0 to destroy; save it with tofu plan -out=PLAN and check it with writ import accept --imports %s PLAN.\n", plural(n, "object"), n, addr.ShellWord(rep.Imports))
		}
	}
	if !rep.OK {
		return ErrFailed
	}
	return nil
}
