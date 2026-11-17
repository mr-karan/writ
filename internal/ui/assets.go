package ui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"slices"
	"strings"
)

//go:embed static/*.css static/*.js static/*.txt
var staticFS embed.FS

// asset is one embedded file, gzipped once at start.
type asset struct {
	body, gz []byte
	ctype    string
}

// assets are the stylesheets and scripts, served at URLs that carry a hash
// of all of them, so a new binary serves new files at new URLs and every
// URL can be cached for a year.
type assets struct {
	hash  string
	files map[string]asset
}

func loadAssets() (*assets, error) {
	names, err := fs.Glob(staticFS, "static/*")
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	a := &assets{files: map[string]asset{}}
	sum := sha256.New()
	for _, n := range names {
		b, err := staticFS.ReadFile(n)
		if err != nil {
			return nil, err
		}
		name := path.Base(n)
		f := asset{body: b, ctype: mime.TypeByExtension(path.Ext(name))}
		if path.Ext(name) == ".js" {
			f.ctype = "text/javascript; charset=utf-8"
		}
		var gz bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
		_, _ = zw.Write(b)
		_ = zw.Close()
		f.gz = gz.Bytes()
		a.files[name] = f
		sum.Write([]byte(name))
		sum.Write(b)
	}
	a.hash = hex.EncodeToString(sum.Sum(nil))[:12]
	return a, nil
}

// url is the path a page links an asset by.
func (a *assets) url(name string) string { return "/ui/static/" + a.hash + "/" + name }

func (a *assets) serve(w http.ResponseWriter, r *http.Request) {
	f, ok := a.files[r.PathValue("file")]
	if !ok || r.PathValue("hash") != a.hash {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("Vary", "Accept-Encoding")
	body := f.body
	if f.gz != nil && acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		body = f.gz
	}
	_, _ = w.Write(body)
}

func acceptsGzip(r *http.Request) bool {
	for part := range strings.SplitSeq(r.Header.Get("Accept-Encoding"), ",") {
		name, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if strings.TrimSpace(name) == "gzip" && strings.TrimSpace(q) != "q=0" {
			return true
		}
	}
	return false
}
