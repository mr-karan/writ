package repo

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/mr-karan/writ/internal/forge"
	"github.com/mr-karan/writ/internal/tofu"
)

// tofuCode reports whether a file is OpenTofu code that this package
// reads: a .tf or .tofu file. JSON code is not read.
func tofuCode(name string) bool {
	ext := path.Ext(name)
	return ext == ".tf" || ext == ".tofu"
}

// tofuJSON reports whether a file is OpenTofu code in JSON, which this
// package does not read, so a stack that holds one is refused.
func tofuJSON(name string) bool {
	return strings.HasSuffix(name, ".tf.json") || strings.HasSuffix(name, ".tofu.json")
}

// isTofuStack reports whether dir holds a lock file and OpenTofu code,
// which makes it a stack the OpenTofu runner plans.
func (s *Snapshot) isTofuStack(dir string) bool {
	return s.held(path.Join(dir, tofu.LockFile)) && s.dirHas(dir, tofuCode)
}

// held reports whether Read read a file, or left it out for its size.
func (s *Snapshot) held(name string) bool {
	_, read := s.Files[name]
	_, big := s.tooLarge[name]
	return read || big
}

// IsTofu reports whether the directory dir holds OpenTofu code, so writ
// serve plans it with OpenTofu. Such a directory without a lock file is no
// stack, and TofuSources says so.
func IsTofu(snap *Snapshot, dir string) bool {
	return snap.dirHas(dir, tofuCode)
}

// dirHas reports whether a file directly in dir, by the commit's tree or
// by the files read, matches.
func (s *Snapshot) dirHas(dir string, match func(string) bool) bool {
	for _, f := range s.Tree {
		if path.Dir(f.Path) == dir && match(f.Path) {
			return true
		}
	}
	for n := range s.Files {
		if path.Dir(n) == dir && match(n) {
			return true
		}
	}
	return false
}

// tofuStackDirs lists the directories of the OpenTofu stacks among the
// files read, and the lock files left out for their size, sorted.
func (s *Snapshot) tofuStackDirs() []string {
	var out []string
	for _, n := range slices.Concat(slices.Collect(maps.Keys(s.Files)), slices.Collect(maps.Keys(s.tooLarge))) {
		if path.Base(n) == tofu.LockFile && s.isTofuStack(path.Dir(n)) && !slices.Contains(out, path.Dir(n)) {
			out = append(out, path.Dir(n))
		}
	}
	slices.Sort(out)
	return out
}

// tofuDirs returns dir and the directories of the local modules its code
// calls, directly or not, sorted. It calls read for each directory before
// it parses the code there. A local module's source starts with ./ or ../
// and names a directory from the calling directory. It refuses a module
// call whose source is not a quoted string, since it could name a local
// directory that would not be gathered, a local path above the
// repository's path, any other source that remoteModuleSource does not
// accept, since tofu init copies or clones a path on the host, and a
// remote source with a query parameter that sourceParam refuses. A file that
// does not parse is skipped: tofu reports it when it runs.
func (s *Snapshot) tofuDirs(dir string, read func(dir string) error) ([]string, error) {
	seen := map[string]bool{dir: true}
	for queue := []string{dir}; len(queue) > 0; queue = queue[1:] {
		d := queue[0]
		if err := read(d); err != nil {
			return nil, err
		}
		for _, name := range slices.Sorted(maps.Keys(s.Files)) {
			if path.Dir(name) != d || !tofuCode(name) {
				continue
			}
			calls, err := moduleCalls(name, s.Files[name])
			if err != nil {
				return nil, err
			}
			for _, mc := range calls {
				src, at := mc.source, fmt.Sprintf("%s:%d", name, mc.line)
				dec, ok := decodedSource(src)
				if !ok {
					return nil, fmt.Errorf("%s: module %q has source %q, which holds a malformed percent escape", at, mc.name, src)
				}
				if strings.Contains(src, `\`) || strings.Contains(dec, `\`) {
					return nil, fmt.Errorf("%s: module %q has source %q; write a local module's path with /, as in ./modules/name", at, mc.name, src)
				}
				if !strings.HasPrefix(src, "./") && !strings.HasPrefix(src, "../") {
					if p, err := sourceParam(src); err != nil {
						return nil, fmt.Errorf("%s: module %q has source %q, whose query does not parse: %w", at, mc.name, src, err)
					} else if p != "" {
						return nil, fmt.Errorf("%s: module %q has source %q, which takes query parameter %q: writ serve allows only ref and depth, since go-getter reads others, such as checksum=file:URL, as orders that can read a file on the server", at, mc.name, src, p)
					}
					if !remoteModuleSource(src, dec) {
						return nil, fmt.Errorf("%s: module %q has source %q, which leaves %s: writ serve takes a module from a path inside it that starts with ./ or ../, a registry address, or a remote address that names a network host", at, mc.name, src, s.root())
					}
					continue
				}
				m := path.Join(d, src)
				if m == ".." || strings.HasPrefix(m, "../") {
					return nil, fmt.Errorf("%s: module %q calls %s, which leaves %s; writ serve reads only the files under it", at, mc.name, src, s.root())
				}
				if !seen[m] {
					seen[m] = true
					queue = append(queue, m)
				}
			}
		}
	}
	return slices.Sorted(maps.Keys(seen)), nil
}

// TofuModuleDirs returns dir and the directories of the local modules its
// code calls, directly or not, sorted, as TofuSources walks the commit
// named commit. read returns the files directly in a directory by name,
// and the walk parses the .tf and .tofu files among them. Directories are
// slash paths from the repository's path.
func TofuModuleDirs(commit, dir string, read func(dir string) (map[string][]byte, error)) ([]string, error) {
	snap := &Snapshot{Commit: commit, Files: map[string][]byte{}}
	return snap.tofuDirs(dir, func(d string) error {
		files, err := read(d)
		for name, text := range files {
			if tofuCode(name) {
				snap.Files[path.Join(d, name)] = text
			}
		}
		return err
	})
}

var (
	forcedGetter   = regexp.MustCompile(`^([A-Za-z0-9]+)::(.+)$`)
	registryName   = regexp.MustCompile(`^[0-9A-Za-z](?:[0-9A-Za-z-_]{0,62}[0-9A-Za-z])?$`)
	registryTarget = regexp.MustCompile(`^[0-9a-z]{1,64}$`)
	registryHost   = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)+(?::[0-9]+)?$`)
	scpLike        = regexp.MustCompile(`^[A-Za-z0-9._-]+@([A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?):(.+)$`)
)

// remoteModuleSource reports whether a module source that is no local
// path is a registry address or names a network host, so tofu init cannot
// take it from the server's files. go-getter turns an absolute path,
// even after a forced getter as in git::/srv/x, into a file:// address
// and copies or clones it, so the check accepts known forms only. dec is
// src percent-decoded, where a file: hidden by escapes shows. A fragment
// is refused: no accepted form needs one, and it moves the end of the
// query that sourceParam reads.
func remoteModuleSource(src, dec string) bool {
	if strings.Contains(strings.ToLower(src), "file:") || strings.Contains(strings.ToLower(dec), "file:") || strings.Contains(src, "#") {
		return false
	}
	if registryModuleSource(src) {
		return true
	}
	if m := forcedGetter.FindStringSubmatch(src); m != nil {
		switch m[1] {
		case "git", "hg", "s3", "gcs", "http", "https":
			src = m[2]
		default:
			return false
		}
	}
	if strings.HasPrefix(src, "github.com/") || strings.HasPrefix(src, "bitbucket.org/") || scpLike.MatchString(src) {
		return true
	}
	u, err := url.Parse(src)
	if err != nil || u.Hostname() == "" {
		return false
	}
	switch u.Scheme {
	case "https", "http", "ssh", "git", "s3", "gcs":
		return true
	}
	return false
}

// decodedSource is src percent-decoded until it stops changing, or false
// when an escape is malformed. go-getter decodes a query value once and
// may hand it to a getter that decodes it again, so one round is not
// enough to see what it reads.
func decodedSource(src string) (string, bool) {
	for {
		d, err := url.PathUnescape(src)
		if err != nil {
			return "", false
		}
		if d == src {
			return d, true
		}
		src = d
	}
}

// sourceParam returns the first query parameter of a remote module source,
// decoded, that is not the git getter's ref or depth, or "" when there is
// none. go-getter's client and getters read other parameters as orders:
// checksum, archive, filename, sshkey, hg's rev, S3's credentials, region,
// version, and aws_profile, which loads the server's AWS profile. A ref
// or depth written with escapes is refused as go-getter reads it.
func sourceParam(src string) (string, error) {
	_, raw, found := strings.Cut(src, "?")
	if !found {
		return "", nil
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return "", err
	}
	for _, k := range slices.Sorted(maps.Keys(q)) {
		d, ok := decodedSource(k)
		if !ok {
			return k, nil
		}
		if d != "ref" && d != "depth" {
			return d, nil
		}
		if d != k {
			return k, nil
		}
	}
	return "", nil
}

// registryModuleSource reports whether src is a module registry address,
// by the syntax of OpenTofu's registry-address package: NAMESPACE/NAME/
// SYSTEM with an optional host before it and //SUBDIR after it.
func registryModuleSource(src string) bool {
	pkg, sub, _ := strings.Cut(src, "//")
	if sub != "" && (path.Clean(sub) == ".." || strings.HasPrefix(path.Clean(sub), "../")) {
		return false
	}
	parts := strings.Split(pkg, "/")
	if len(parts) == 4 {
		host := strings.ToLower(parts[0])
		if !registryHost.MatchString(host) || host == "github.com" || host == "bitbucket.org" {
			return false
		}
		parts = parts[1:]
	}
	return len(parts) == 3 && registryName.MatchString(parts[0]) && registryName.MatchString(parts[1]) && registryTarget.MatchString(parts[2])
}

// moduleCall is one module block: its name, its source, and the line of
// its source.
type moduleCall struct {
	name, source string
	line         int
}

// moduleCalls returns the module blocks of a file that set a source. A
// source that is not a quoted string with no interpolation is an error.
func moduleCalls(name string, text []byte) ([]moduleCall, error) {
	f, diags := hclsyntax.ParseConfig(text, name, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, nil
	}
	var out []moduleCall
	for _, b := range f.Body.(*hclsyntax.Body).Blocks {
		a := b.Body.Attributes["source"]
		if b.Type != "module" || a == nil {
			continue
		}
		label := strings.Join(b.Labels, " ")
		src, ok := literalString(a)
		if !ok {
			return nil, fmt.Errorf("%s:%d: module %q takes its source from an expression; writ serve gathers a stack's local modules before OpenTofu runs, so write the source as a quoted string, such as \"./modules/name\"", name, a.SrcRange.Start.Line, label)
		}
		out = append(out, moduleCall{name: label, source: src, line: a.SrcRange.Start.Line})
	}
	return out, nil
}

// literalString is the value of an attribute that is a quoted string with
// no interpolation.
func literalString(a *hclsyntax.Attribute) (string, bool) {
	t, ok := a.Expr.(*hclsyntax.TemplateExpr)
	if !ok || !t.IsStringLiteral() {
		return "", false
	}
	v, diags := t.Value(nil)
	if diags.HasErrors() || !v.IsKnown() || v.IsNull() {
		return "", false
	}
	return v.AsString(), true
}

// under reports whether name is in one of dirs or below it.
func under(name string, dirs []string) bool {
	for _, d := range dirs {
		if d == "." || strings.HasPrefix(name, d+"/") {
			return true
		}
	}
	return false
}

// TofuStack is one OpenTofu stack of a commit, as a plan takes it.
type TofuStack struct {
	// Dir is the stack's directory from the repository's path.
	Dir string
	// Files are every file in the stack's directory and in the directories
	// of the local modules it calls, and below them, by path from the
	// repository's path.
	Files map[string][]byte
	// Lock is the stack's .terraform.lock.hcl.
	Lock []byte
	// Backend is the backend block of the stack's terraform block, or
	// nil when it has none.
	Backend *Backend
	// Cloud is set when the terraform block holds a cloud block.
	Cloud bool
	// Digest is the SHA-256 of Files, which a plan of the stack records.
	Digest string
}

// Backend is a stack's backend block: its type and the names of the
// attributes and blocks it sets.
type Backend struct {
	Type string
	Set  []string
	// Literals are the attributes whose value is a literal string, number,
	// or bool, as text. An attribute set by an expression, such as a
	// variable, is not here.
	Literals map[string]string
	// Objects are the attributes whose value is an object of literal
	// strings, such as an S3 backend's endpoints, each key to its text.
	Objects map[string]map[string]string
}

// TofuSources gathers the OpenTofu stack in dir for a plan: its files, the
// files of the local modules it calls, its lock file, and its backend.
// Read read the lock files of the commit; TofuSources reads the stack's
// other files from the store or the forge and adds them to the snapshot,
// whose Keep stores the blobs it fetched. It refuses a directory with no
// lock file, a module call tofuDirs refuses, a .terraform directory among
// the files, code in JSON in the stack's directory or a module's, which it
// cannot read for module calls, code that does not parse in the stack's
// directory, and a stack over a limit: a source file over lim.File, any
// other file over lim.StackFile, or all its files over lim.Files or
// lim.StackBytes. Every file counts, whether the snapshot held it already
// or not.
func TofuSources(ctx context.Context, c forge.Client, projectID string, snap *Snapshot, dir string, st Store, lim Limits) (TofuStack, error) {
	if dir == "" || path.Clean(dir) != dir || strings.HasPrefix(dir, "../") || dir == ".." {
		return TofuStack{}, fmt.Errorf("stack directory %q is not a clean path in the repository", dir)
	}
	if !IsTofu(snap, dir) {
		return TofuStack{}, fmt.Errorf("%s has no OpenTofu stack in %s, which needs .tf or .tofu files", snap.what(), dir)
	}
	if !snap.held(path.Join(dir, tofu.LockFile)) {
		return TofuStack{}, fmt.Errorf("%s has no %s in %s: run tofu providers lock in the stack's directory, and commit the file", snap.what(), tofu.LockFile, dir)
	}
	tooMany := fmt.Errorf("stack %s and its local modules hold over %d files in %s", dir, lim.Files, snap.what())
	counted := map[string]bool{}
	var total int64
	count := func(f forge.File, n int64) error {
		if counted[f.Path] {
			return nil
		}
		counted[f.Path] = true
		if len(counted) > lim.Files {
			return tooMany
		}
		if total += n; total > lim.StackBytes {
			return fmt.Errorf("the files of stack %s and its local modules in %s are over %d bytes, with %s; build large artifacts outside the repository", dir, snap.what(), lim.StackBytes, f.Path)
		}
		return nil
	}
	over := func(f forge.File, max int64) error { return overStackFile(snap, f, max) }
	gather := func(files []forge.File) error {
		var missing []forge.File
		for _, f := range files {
			if max, big := snap.tooLarge[f.Path]; big {
				return overStackFile(snap, f, max)
			}
			if text, ok := snap.Files[f.Path]; !ok {
				missing = append(missing, f)
			} else if err := count(f, int64(len(text))); err != nil {
				return err
			}
		}
		if len(counted)+len(missing) > lim.Files {
			return tooMany
		}
		return snap.load(ctx, c, projectID, missing, st, lim.stackFile, over, count)
	}
	dirs, err := snap.tofuDirs(dir, func(d string) error {
		var code []forge.File
		for _, f := range snap.Tree {
			if path.Dir(f.Path) != d {
				continue
			}
			if tofuJSON(f.Path) {
				return fmt.Errorf("%s holds %s: writ serve reads a stack's and its modules' code in .tf and .tofu files only", snap.what(), f.Path)
			}
			if tofuCode(f.Path) {
				code = append(code, f)
			}
		}
		return gather(code)
	})
	if err != nil {
		return TofuStack{}, err
	}
	var files []forge.File
	for _, f := range snap.Tree {
		if !under(f.Path, dirs) {
			continue
		}
		if slices.Contains(strings.Split(f.Path, "/"), ".terraform") {
			return TofuStack{}, fmt.Errorf("%s holds %s, in a .terraform directory, which tofu init makes and which must not be committed", snap.what(), f.Path)
		}
		files = append(files, f)
	}
	if err := gather(files); err != nil {
		return TofuStack{}, err
	}
	ts := TofuStack{Dir: dir, Files: map[string][]byte{}, Lock: snap.Files[path.Join(dir, tofu.LockFile)]}
	for _, f := range files {
		ts.Files[f.Path] = snap.Files[f.Path]
	}
	for _, name := range slices.Sorted(maps.Keys(ts.Files)) {
		if path.Dir(name) != dir || !tofuCode(name) {
			continue
		}
		f, diags := hclsyntax.ParseConfig(ts.Files[name], name, hcl.InitialPos)
		if diags.HasErrors() {
			return TofuStack{}, fmt.Errorf("%s", diags.Error())
		}
		for _, tb := range f.Body.(*hclsyntax.Body).Blocks {
			if tb.Type != "terraform" {
				continue
			}
			for _, b := range tb.Body.Blocks {
				switch b.Type {
				case "cloud":
					ts.Cloud = true
				case "backend":
					if ts.Backend != nil {
						return TofuStack{}, fmt.Errorf("%s: the stack declares a second backend; a stack has one", name)
					}
					be := &Backend{Set: []string{}, Literals: map[string]string{}, Objects: map[string]map[string]string{}}
					if len(b.Labels) == 1 {
						be.Type = b.Labels[0]
					}
					for name, a := range b.Body.Attributes {
						be.Set = append(be.Set, name)
						if text, ok := literal(a.Expr); ok {
							be.Literals[name] = text
						} else if obj, ok := literalObject(a.Expr); ok {
							be.Objects[name] = obj
						}
					}
					for _, inner := range b.Body.Blocks {
						be.Set = append(be.Set, inner.Type)
					}
					slices.Sort(be.Set)
					ts.Backend = be
				}
			}
		}
	}
	ts.Digest = tofuDigest(ts.Files)
	return ts, nil
}

// literal is the value of an expression that needs no evaluation context,
// as text, when it is a string, a number, or a bool.
func literal(e hclsyntax.Expression) (string, bool) {
	v, diags := e.Value(nil)
	if diags.HasErrors() || !v.IsKnown() || v.IsNull() {
		return "", false
	}
	// The type is matched by name: only internal/provider imports cty.
	switch v.Type().FriendlyName() {
	case "string":
		return v.AsString(), true
	case "bool":
		return strconv.FormatBool(v.True()), true
	case "number":
		return v.AsBigFloat().Text('f', -1), true
	}
	return "", false
}

// literalObject is the value of an object expression that needs no
// evaluation context and holds only strings, each key to its text.
func literalObject(e hclsyntax.Expression) (map[string]string, bool) {
	v, diags := e.Value(nil)
	if diags.HasErrors() || !v.IsWhollyKnown() || v.IsNull() || !v.Type().IsObjectType() {
		return nil, false
	}
	out := map[string]string{}
	for it := v.ElementIterator(); it.Next(); {
		k, el := it.Element()
		if el.IsNull() || el.Type().FriendlyName() != "string" {
			return nil, false
		}
		out[k.AsString()] = el.AsString()
	}
	return out, true
}

// tofuSourceFile reports whether a file of an OpenTofu stack is held to the
// limit on one estate file: code, variables, and the lock file.
func tofuSourceFile(name string) bool {
	return tofuCode(name) || path.Ext(name) == ".tfvars" || path.Base(name) == tofu.LockFile
}

// stackFile is the limit on one file of an OpenTofu stack.
func (lim Limits) stackFile(f forge.File) int64 {
	if tofuSourceFile(f.Path) {
		return lim.File
	}
	return lim.StackFile
}

// overStackFile refuses a stack for one file over max bytes.
func overStackFile(snap *Snapshot, f forge.File, max int64) error {
	if tofuSourceFile(f.Path) {
		return fmt.Errorf("%s of %s is over %d bytes, the limit on a stack's .tf, .tofu, .tfvars, and lock files", f.Path, snap.what(), max)
	}
	return fmt.Errorf("%s of %s is over %d bytes, the limit on a stack's other files; build large artifacts outside the repository", f.Path, snap.what(), max)
}

// tofuDigest is the SHA-256 of files in name order. Every name and text is
// length-prefixed, after a tag that no bundle digest starts with.
func tofuDigest(files map[string][]byte) string {
	h := sha256.New()
	part := func(b []byte) {
		h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(b))))
		h.Write(b)
	}
	part([]byte("opentofu"))
	h.Write(binary.BigEndian.AppendUint64(nil, uint64(len(files))))
	for _, name := range slices.Sorted(maps.Keys(files)) {
		part([]byte(name))
		part(files[name])
	}
	return hex.EncodeToString(h.Sum(nil))
}
