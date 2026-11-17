package ui

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/mr-karan/writ/internal/server"
)

// TestVendor checks each vendored file against the SHA-256 in VENDOR.
func TestVendor(t *testing.T) {
	f, err := os.Open("static/VENDOR")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 5 {
			t.Fatalf("VENDOR line %q is not file, project, version, URL, SHA-256", line)
		}
		b, err := os.ReadFile("static/" + fields[0])
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != fields[4] {
			t.Errorf("%s has SHA-256 %s, VENDOR says %s", fields[0], got, fields[4])
		}
		if !strings.Contains(fields[3], "@"+fields[2]+"/") {
			t.Errorf("%s: the URL %s does not name version %s", fields[0], fields[3], fields[2])
		}
		n++
	}
	if n != 2 {
		t.Fatalf("VENDOR lists %d files, want Oat's stylesheet and its license", n)
	}
}

// TestAssetSize keeps the stylesheets and scripts under 24 KB gzipped in
// all, the design's budget.
func TestAssetSize(t *testing.T) {
	a, err := loadAssets()
	if err != nil {
		t.Fatal(err)
	}
	text := 0
	for name, f := range a.files {
		t.Logf("%s: %d bytes, %d gzipped", name, len(f.body), len(f.gz))
		text += len(f.gz)
	}
	t.Logf("stylesheets, scripts, and licenses gzipped: %d bytes", text)
	if text > 24<<10 {
		t.Fatalf("the stylesheets, scripts, and licenses are %d bytes gzipped, over 24 KB", text)
	}
	js, _ := os.ReadFile("static/writ.js")
	if lines := bytes.Count(js, []byte("\n")); lines > 400 {
		t.Fatalf("writ.js has %d lines, over 400", lines)
	}
}

// TestEveryAssetIsUsed keeps out a file no page loads: a template, a
// stylesheet, or the UI's code names each embedded file.
func TestEveryAssetIsUsed(t *testing.T) {
	a, err := loadAssets()
	if err != nil {
		t.Fatal(err)
	}
	var users []byte
	for _, glob := range []string{"templates/*.html", "static/*.css", "*.go"} {
		names, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if strings.HasSuffix(n, "_test.go") {
				continue
			}
			b, err := os.ReadFile(n)
			if err != nil {
				t.Fatal(err)
			}
			users = append(users, b...)
		}
	}
	for name := range a.files {
		if !bytes.Contains(users, []byte(`"`+name+`"`)) {
			t.Errorf("%s is embedded, and no template, stylesheet, or code names it", name)
		}
	}
}

// pairs reads every light-dark() custom property of a stylesheet.
func pairs(t *testing.T, file string) map[string][2]string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][2]string{}
	re := regexp.MustCompile(`(--[a-z-]+):\s*light-dark\(\s*(#[0-9a-fA-F]{6}|#[0-9a-fA-F]{3})\s*,\s*(#[0-9a-fA-F]{6}|#[0-9a-fA-F]{3})\s*\)`)
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		out[m[1]] = [2]string{long(m[2]), long(m[3])}
	}
	return out
}

// long writes #abc as #aabbcc.
func long(c string) string {
	if len(c) == 4 {
		return "#" + strings.Repeat(c[1:2], 2) + strings.Repeat(c[2:3], 2) + strings.Repeat(c[3:4], 2)
	}
	return c
}

func luminance(hexColor string) float64 {
	v, _ := strconv.ParseUint(hexColor[1:], 16, 32)
	ch := func(c uint64) float64 {
		s := float64(c) / 255
		if s <= 0.04045 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(v>>16&0xff) + 0.7152*ch(v>>8&0xff) + 0.0722*ch(v&0xff)
}

func contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	return (max(la, lb) + 0.05) / (min(la, lb) + 0.05)
}

// TestContrast computes the WCAG 2.2 contrast of every text color the
// pages put on every background they put it on, in both themes, from
// Oat's stylesheet and Writ's, which overrides it.
func TestContrast(t *testing.T) {
	c := pairs(t, "static/oat.min.css")
	for k, v := range pairs(t, "static/writ.css") {
		c[k] = v
	}
	grounds := []string{"--background", "--card", "--muted", "--faint", "--side", "--side-active", "--err-bg", "--add-bg", "--chg-bg", "--run-bg"}
	text := []struct {
		fg  string
		bgs []string
	}{
		{"--foreground", grounds},
		{"--muted-foreground", grounds},
		{"--primary", []string{"--background", "--card", "--faint", "--side"}},
		{"--primary-foreground", []string{"--primary"}},
		{"--add", []string{"--background", "--card", "--add-bg"}},
		{"--del", []string{"--background", "--card", "--err-bg"}},
		{"--chg", []string{"--background", "--card", "--chg-bg"}},
		{"--mov", []string{"--background", "--card", "--run-bg"}},
	}
	for i, theme := range []string{"light", "dark"} {
		for _, tc := range text {
			for _, bg := range tc.bgs {
				f, okf := c[tc.fg]
				g, okg := c[bg]
				if !okf || !okg {
					t.Fatalf("no light-dark() color for %s or %s", tc.fg, bg)
				}
				if r := contrast(f[i], g[i]); r < 4.5 {
					t.Errorf("%s: %s %s on %s %s is %.2f:1, under 4.5:1", theme, tc.fg, f[i], bg, g[i], r)
				}
			}
		}
		// The focus ring, the borders that mark a change, the borders of
		// fields, and the status dots are at least 3:1 against the page,
		// the cards, and the sidebar (WCAG 1.4.11).
		for _, mark := range []string{"--ring", "--del", "--chg", "--add", "--mov", "--input", "--rule-strong"} {
			for _, bg := range []string{"--background", "--card", "--side", "--side-active"} {
				if r := contrast(c[mark][i], c[bg][i]); r < 3 {
					t.Errorf("%s: %s on %s is %.2f:1, under 3:1", theme, mark, bg, r)
				}
			}
		}
	}
}

func TestSafeNext(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/ui/plans?env=prod", "/ui/plans?env=prod"},
		{"/ui/", "/ui/"},
		{"/ui/plans/", "/ui/plans/"},
		{"/ui/../v1/config", "/ui/"},
		{"/ui/%2e%2e/v1", "/ui/"},
		{"https://evil.example.com/ui/", "/ui/"},
		{"//evil.example.com/ui/", "/ui/"},
		{"/ui/login?next=/ui/", "/ui/"},
		{"/ui/callback", "/ui/"},
		{"/ui\\evil", "/ui/"},
		{"/\\evil.example.com/ui/", "/ui/"},
		{"\\\\evil.example.com/ui/", "/ui/"},
		{"/ui/%5C%5Cevil.example.com", "/ui/%5C%5Cevil.example.com"},
		{"/ui/%2F%2Fevil.example.com", "/ui/evil.example.com"},
		{"/\t/evil.example.com/ui/", "/ui/"},
		{"https:/ui/", "/ui/"},
		{"javascript:/ui/", "/ui/"},
		{" /ui/", "/ui/"},
		{"/ui/plans#x", "/ui/plans"},
		{"/v1/whoami", "/ui/"},
		{"", "/ui/"},
	}
	for _, tt := range tests {
		if got := safeNext(tt.in); got != tt.want {
			t.Errorf("safeNext(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// FuzzPlanPage renders plan review, of an OpenTofu plan, a refusal, and a
// plan of an older Writ, with a random string in every field a document
// controls, and checks that the string never becomes markup: no
// script, no event handler, no style attribute, no javascript: link.
func FuzzPlanPage(f *testing.F) {
	for _, s := range []string{`<script>alert(1)</script>`, `"><img src=x onerror=alert(1)>`, `javascript:alert(1)`, `</pre><style>`, "\x00 ", `{"a":"<b>"}`} {
		f.Add(s)
	}
	a, err := loadAssets()
	if err != nil {
		f.Fatal(err)
	}
	u := &UI{assets: a, now: time.Now}
	if u.tmpl, err = parseTemplates(u); err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, s string) {
		q, _ := json.Marshal(s)
		info := &server.PlanInfo{ID: "3f9c2a7e5b1d48c09e6a2f71b4d80001", Status: "pending", Env: s, Stacks: []server.PlanStack{{Stack: s}},
			Author: server.Principal{Display: s, Person: s}, Risks: []string{s}, TeamsThatMayApply: []string{s}, Origin: json.RawMessage(`{}`)}
		v := &planView{Plan: info, Text: s, Page: 1, Older: true, Diagnostics: []docDiag{{Severity: "error", Class: s, File: s, Message: s}}}
		tofu, err := readTofuPlan(json.RawMessage(`{"format_version":"1.2","resource_changes":[{"address":` + string(q) + `,"module_address":` + string(q) +
			`,"type":` + string(q) + `,"change":{"actions":["update"],"before":{"a":` + string(q) + `},"after":{"a":"x"}}}]}`))
		if err != nil {
			t.Fatal(err)
		}
		if v.Tofu, err = newTofuView(tofu, v); err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := u.tmpl.ExecuteTemplate(&b, "plan", &page{Title: s, Theme: "system", Session: &server.Session{Display: s}, Data: v}); err != nil {
			t.Fatal(err)
		}
		root, err := html.Parse(&b)
		if err != nil {
			t.Fatal(err)
		}
		var walk func(*html.Node)
		walk = func(n *html.Node) {
			if n.Type == html.ElementNode {
				if n.Data == "script" {
					src := ""
					for _, at := range n.Attr {
						if at.Key == "src" {
							src = at.Val
						}
					}
					if !strings.HasPrefix(src, "/ui/static/") || n.FirstChild != nil {
						t.Fatalf("%q made a script", s)
					}
				}
				if n.Data == "style" || n.Data == "img" || n.Data == "iframe" {
					t.Fatalf("%q made a <%s>", s, n.Data)
				}
				for _, at := range n.Attr {
					if at.Key == "style" || strings.HasPrefix(at.Key, "on") || (at.Key == "href" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(at.Val)), "javascript:")) {
						t.Fatalf("%q made the attribute %s=%q on <%s>", s, at.Key, at.Val, n.Data)
					}
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
		}
		walk(root)
	})
}

// The approve button of a commit plan by someone else shows only when the
// page could read the plan's document, and never for a plan an older Writ
// made, which says so.
func TestApproveNeedsReadableDocument(t *testing.T) {
	a, err := loadAssets()
	if err != nil {
		t.Fatal(err)
	}
	u := &UI{assets: a, now: time.Now}
	if u.tmpl, err = parseTemplates(u); err != nil {
		t.Fatal(err)
	}
	doc := []byte(`{"format_version":"1.2","resource_changes":[{"address":"random_pet.old","change":{"actions":["delete"]}}]}`)
	for _, tc := range []struct {
		name    string
		doc     []byte
		approve bool
	}{
		{"readable", doc, true},
		{"unreadable", []byte(strings.Replace(string(doc), `"delete"`, `"explode"`, 1)), false},
		{"older Writ", []byte(`{"format":"writ.plan.json/v2","ok":true,"summary":{"create":1},"changes":[{"addr":"pet","action":"create"}]}`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &server.PlanInfo{ID: strings.Repeat("a", 32), Status: "pending", Env: "prod", Stacks: []server.PlanStack{{Stack: "app"}},
				Author: server.Principal{Person: "gitlab/user/1001"}, Source: server.PlanSourceInfo{Kind: "commit"}}
			v := &planView{Plan: info, Page: 1}
			tofu, diags, older, docErr := readPlanDoc(tc.doc)
			v.Diagnostics, v.Older = diags, older
			if docErr == nil && tofu != nil {
				v.Tofu, docErr = newTofuView(tofu, v)
			}
			if docErr != nil {
				v.DocError = docErr.Error()
			}
			var b bytes.Buffer
			if err := u.tmpl.ExecuteTemplate(&b, "plan", &page{Session: &server.Session{Person: "gitlab/user/2002"}, Data: v}); err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(b.String(), "Approve this plan"); got != tc.approve {
				t.Fatalf("the page offers an approval: %v, want %v\n%s", got, tc.approve, b.String())
			}
			if got := strings.Contains(b.String(), "An older Writ made this plan"); got != (tc.name == "older Writ") {
				t.Fatalf("the page says an older Writ made the plan: %v\n%s", got, b.String())
			}
		})
	}
}
