package ui_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode"

	"golang.org/x/net/html"

	"github.com/mr-karan/writ/internal/control/controltest"
	"github.com/mr-karan/writ/internal/server"
	"github.com/mr-karan/writ/internal/ui"
)

// machineText are the elements whose text is an identifier or a value,
// not a count, so a digit in them is the identifier's own: a timestamp or
// a duration (time), an id, a commit, a command, an address, a version, a
// port, an HTTP status, or a configured value (code, pre, .id, .id-full).
var machineText = []func(*html.Node) bool{
	isTag("time"), isTag("code"), isTag("pre"), classed("id"), classed("id-full"),
}

// servedText are the elements whose text writ serve or OpenTofu wrote, or
// a name set in prose: a diagnostic, an error, or an alert's detail
// (.msg), and a person's display name or a project (.ref). A digit in one
// stands only when its whole text, never a number alone, is a given name
// or the field of the one record its data-record picks, so a template
// cannot pass a count off as one.
var servedText = []func(*html.Node) bool{classed("ref"), classed("msg")}

// fixedPhrases are the numbers the templates write in words of their own:
// the limits of a read, the windows of an alert, the reload of a page, and
// the version of the stylesheet the UI embeds.
var fixedPhrases = regexp.MustCompile(`\b(first 200|newest 200|more than 200|(Previous|Next) 200|24 hours|(3|5|30) seconds|10 minutes|Oat 1\.0\.0)\b`)

// readableAttrs are the attributes a person reads or hears. Every other
// attribute is for the browser: a URL, an id, a class, a geometry.
var readableAttrs = []string{"aria-label", "aria-description", "aria-valuetext", "title", "alt", "placeholder"}

// noneWords are the words by which a sentence says that a thing is absent
// or that all is well. The UI ships in English only, so the check reads
// English: a sentence in another language would slip past it.
var noneWords = regexp.MustCompile(`(?i)\b(no|nothing|none|never|clean|all clear)\b`)

// terms are the phrases that hold a word of noneWords and say nothing is
// absent: a clean work tree is one with every change committed.
var terms = regexp.MustCompile(`\bclean work tree\b`)

// fieldsRE are the listings and fields of a page, as template text: each
// row or field says what one record holds, so its own words, such as no
// commit or never checked, are that record's.
var fieldsRE = regexp.MustCompile(`(?s)<table\b.*?</table>|<ul class="(rows|alerts)">.*?</ul>|<dl\b.*?</dl>`)

// fields are the elements fieldsRE finds, on a rendered page, and the
// tree's notes, whose words checkRender judges one by one.
func fields(n *html.Node) bool {
	return n.Data == "table" || n.Data == "dl" || n.Data == "ul" && (hasClass(n, "rows") || hasClass(n, "alerts")) || hasClass(n, "t-note")
}

// noneScope are the pages whose own words checkWords holds to func none,
// by path. The sidebar is held on every page. The pages of one plan, one
// run, sign-in, an error, and the repository form say what one record or
// one request holds.
func noneScope(path string) bool {
	p, _, _ := strings.Cut(path, "?")
	switch p {
	case "/ui/", "/ui/approvals", "/ui/plans", "/ui/runs", "/ui/stack", "/ui/repos", "/ui/access", "/ui/whoami", "/ui/settings", "/ui/start":
		return true
	}
	return strings.HasPrefix(p, "/ui/repos/") && p != "/ui/repos/new"
}

// reporter is where the render check reports: a *testing.T, or a test
// of the check itself.
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// judge reports whether the sentence of nones under key holds where the
// page says it, at, the block that holds it.
type judge func(key string, at *html.Node) bool

// checkWords is the render check every page passes. Every digit the page
// writes, in its text, its title, and the attributes a person reads, is in
// a number of template n, in machineText, in a name of names, in a fixed
// phrase, or in the filter's own text quoted; in servedText only as a
// name, or as the field of the one record of served, the answers writ
// serve gave the page's handler, that its data-record picks. Every
// data-record parses and names its record by values the page shows. A
// number of n is a count, with a + when it is a floor.
// Each sentence of nones on the page holds by say, when say is not nil,
// and in the sidebar and the main part of a page of noneScope no word of
// noneWords is said outside func none, a listing, a field, or a message.
func checkWords(t reporter, path string, doc *html.Node, served [][]byte, names []string, say judge) {
	t.Helper()
	if doc == nil {
		t.Errorf("(w) %s: not HTML", path)
		return
	}
	c := wordCheck{t: t, path: path, names: byLength(names), served: served}
	if u, err := url.Parse(path); err == nil {
		c.find = strings.Join(strings.Fields(u.Query().Get("find")), " ")
	}
	c.ids = c.idTexts(doc)
	main := first(doc, func(n *html.Node) bool { id, _ := attr(n, "id"); return id == "main" })
	for _, b := range c.blocks(doc) {
		c.digits(b)
		if up(b.node, classed("side")) != nil || noneScope(path) && main != nil && (b.node == main || up(b.node, func(n *html.Node) bool { return n == main }) != nil) {
			c.nones(b, say)
		}
	}
	c.attrs(doc)
	c.shown(doc)
}

type wordCheck struct {
	t     reporter
	path  string
	names []string
	// ids are the texts of the page's identifiers: a title or a readable
	// attribute may name one, such as the plan id of "Plan 2fbc4036".
	ids map[string]bool
	// find is the filter's text, which the sidebar quotes and its field
	// holds.
	find string
	// served are the answers writ serve gave the page's handler, and docs
	// those that parse, once an element of servedText needs them.
	served [][]byte
	docs   []any
}

func (c *wordCheck) errorf(format string, args ...any) {
	c.t.Helper()
	c.t.Errorf("(w) %s: "+format, append([]any{c.path}, args...)...)
}

func byLength(names []string) []string {
	out := slices.Clone(names)
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// idTexts are the whole texts of the page's id, commit, and code elements.
func (c *wordCheck) idTexts(doc *html.Node) map[string]bool {
	ids := map[string]bool{}
	for _, n := range everyOf(doc, func(n *html.Node) bool { return n.Data == "code" || hasClass(n, "id") || hasClass(n, "id-full") }) {
		ids[folded(n)] = true
	}
	return ids
}

// wordBlock is the text a paragraph, heading, item, cell, or field writes
// in its own words: its text outside numbers of n and machineText, each
// left out run marked by a space.
type wordBlock struct {
	node *html.Node
	text string
	// inField reports a block inside a listing or a field.
	inField bool
}

var blockTags = []string{"p", "h1", "h2", "h3", "h4", "h5", "h6", "li", "summary", "caption", "td", "th", "dt", "dd", "legend", "label", "button", "option", "a", "title", "span", "div", "main", "nav", "section", "article", "body"}

func (c *wordCheck) blocks(doc *html.Node) []wordBlock {
	var out []wordBlock
	at := map[*html.Node]int{}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch {
			case slices.Contains([]string{"script", "style", "svg"}, n.Data):
				return
			case hasClass(n, "n"):
				if _, ok := readFig(n); !ok {
					c.errorf("template n wrote %q", folded(n))
				}
				c.gap(n, at, &out)
				return
			case slices.ContainsFunc(machineText, func(f func(*html.Node) bool) bool { return f(n) }):
				c.gap(n, at, &out)
				return
			case slices.ContainsFunc(servedText, func(f func(*html.Node) bool) bool { return f(n) }):
				c.servedText(n)
				c.gap(n, at, &out)
				return
			}
		}
		if n.Type == html.TextNode && strings.TrimSpace(n.Data) != "" {
			b := c.blockOf(n, at, &out)
			out[b].text += n.Data
		}
		for k := n.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(doc)
	for i := range out {
		out[i].text = strings.Join(strings.Fields(out[i].text), " ")
	}
	return out
}

// blockOf is the index of the block that holds n, which it adds when new.
func (c *wordCheck) blockOf(n *html.Node, at map[*html.Node]int, out *[]wordBlock) int {
	b := up(n, func(a *html.Node) bool { return slices.Contains(blockTags, a.Data) })
	i, ok := at[b]
	if !ok {
		i = len(*out)
		at[b] = i
		*out = append(*out, wordBlock{node: b, inField: up(n, fields) != nil})
	}
	return i
}

// gap marks where a left out run stood, so the words on its two sides do
// not join.
func (c *wordCheck) gap(n *html.Node, at map[*html.Node]int, out *[]wordBlock) {
	b := c.blockOf(n, at, out)
	(*out)[b].text += " · "
}

// strip drops the names, the filter's own text where the sidebar quotes
// it, and the fixed phrases from s.
func (c *wordCheck) strip(s string) string {
	return fixedPhrases.ReplaceAllString(c.stripNames(s), " ")
}

// stripNames removes the filter's text and the given names, but no fixed
// phrase: a title or a readable attribute holds a digit only in a name or
// an identifier the page shows.
func (c *wordCheck) stripNames(s string) string {
	if c.find != "" {
		s = strings.ReplaceAll(s, "“"+c.find+"”", " ")
	}
	for _, name := range c.names {
		s = replaceWord(s, name)
	}
	return s
}

// servedFields are the fields an element of servedText may render, by
// its class: a message or an error for .msg, a display name or a project
// for .ref.
var servedFields = map[string][]string{"msg": {"message", "error"}, "ref": {"display", "project", "repository"}}

// servedText judges the text of an element of servedText. Its text
// with a digit is never a number alone, and is a name, or the field its
// data-field names of the one record its own or an ancestor's data-record
// picks in the documents the page's handler read.
func (c *wordCheck) servedText(n *html.Node) {
	c.t.Helper()
	s := folded(n)
	if !strings.ContainsAny(s, "0123456789") {
		return
	}
	kind := "ref"
	if hasClass(n, "msg") {
		kind = "msg"
	}
	if !strings.ContainsFunc(s, unicode.IsLetter) {
		c.errorf("writes %q, a number, as a .%s", s, kind)
		return
	}
	if slices.Contains(c.names, s) {
		return
	}
	field, _ := attr(n, "data-field")
	segs := strings.Split(field, ".")
	if !slices.Contains(servedFields[kind], strings.TrimSuffix(segs[len(segs)-1], "[]")) {
		c.errorf("writes %q as a .%s whose data-field %q is none of %q", s, kind, field, servedFields[kind])
		return
	}
	at := n
	for at != nil && !hasAttr(at, "data-record") {
		at = at.Parent
	}
	if at == nil {
		c.errorf("writes %q as a .%s with no data-record", s, kind)
		return
	}
	q, _ := attr(at, "data-record")
	where, ok := parseRecord(q)
	if !ok {
		c.errorf("writes %q as a .%s whose data-record %q is malformed", s, kind, q)
		return
	}
	recs := c.records(where)
	if len(recs) != 1 {
		c.errorf("writes %q as a .%s whose data-record %q picks %d records of the documents the page read, not one", s, kind, q, len(recs))
		return
	}
	if !slices.Contains(fieldValues(recs[0], segs), s) {
		c.errorf("writes %q as field %q of record %q, which holds it not there", s, field, q)
	}
}

// parseRecord parses a data-record: one or more pairs of a key and a
// value, neither empty, each key once.
func parseRecord(q string) (url.Values, bool) {
	where, err := url.ParseQuery(q)
	if err != nil || len(where) == 0 {
		return nil, false
	}
	for k, vs := range where {
		if k == "" || len(vs) != 1 || vs[0] == "" {
			return nil, false
		}
	}
	return where, true
}

// docFormat reports whether v is the top-level format of a document the
// page's handler read. A format nested inside a document names a record,
// not a document, so it is not a type.
func (c *wordCheck) docFormat(v string) bool {
	c.records(url.Values{})
	for _, doc := range c.docs {
		if m, ok := doc.(map[string]any); ok && m["format"] == v {
			return true
		}
	}
	return false
}

// records are the objects of the documents the page's handler read whose
// value at each key of where, a path of fields joined by dots, is the
// string where holds.
func (c *wordCheck) records(where url.Values) []map[string]any {
	if c.docs == nil {
		c.docs = []any{}
		for _, body := range c.served {
			var doc any
			if json.Unmarshal(body, &doc) == nil {
				c.docs = append(c.docs, doc)
			}
		}
	}
	var out []map[string]any
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case []any:
			for _, x := range v {
				walk(x)
			}
		case map[string]any:
			if picked(v, where) {
				out = append(out, v)
			}
			for _, x := range v {
				walk(x)
			}
		}
	}
	for _, doc := range c.docs {
		walk(doc)
	}
	// Copies of one record, equal in every field, such as a plan that
	// both the plan list and the overview hold, count as one.
	seen := map[string]bool{}
	return slices.DeleteFunc(out, func(rec map[string]any) bool {
		b, _ := json.Marshal(rec)
		dup := seen[string(b)]
		seen[string(b)] = true
		return dup
	})
}

func hasAttr(n *html.Node, key string) bool {
	_, ok := attr(n, key)
	return ok
}

func picked(obj map[string]any, where url.Values) bool {
	for k := range where {
		var v any = obj
		for _, step := range strings.Split(k, ".") {
			m, _ := v.(map[string]any)
			v = m[step]
		}
		if s, ok := v.(string); !ok || s != where.Get(k) {
			return false
		}
	}
	return true
}

// fieldValues are the strings at path in rec. A step of path that ends
// in [] goes through each element of a list.
func fieldValues(rec map[string]any, path []string) []string {
	var out []string
	var at func(v any, path []string)
	at = func(v any, path []string) {
		if len(path) == 0 {
			if s, ok := v.(string); ok {
				out = append(out, strings.Join(strings.Fields(s), " "))
			}
			return
		}
		m, _ := v.(map[string]any)
		key, each := strings.CutSuffix(path[0], "[]")
		if !each {
			at(m[key], path[1:])
			return
		}
		list, _ := m[key].([]any)
		for _, x := range list {
			at(x, path[1:])
		}
	}
	at(rec, path)
	return out
}

// types are the values a data-record may name that the page need not
// show, since they say what type a record is, not which one: the kind of
// an alert, which the page says in words of its own, and the format of a
// document a page renders a part of.
var types = map[string][]string{
	"kind":   {"drifted", "drift-check-failed", "outside-write", "run-failed", "plan-expiring", "repo-poll-failed"},
	"format": {server.FormatRemotePlan, server.FormatTofuApply, server.FormatHealth, server.FormatError},
}

// shown fails each data-record of the page that is malformed or that
// names a record by a value the page does not show: every value is in
// the page's text or in one of its links, but a value of types.
func (c *wordCheck) shown(doc *html.Node) {
	c.t.Helper()
	body := first(doc, isTag("body"))
	if body == nil {
		return
	}
	visible := text(body)
	var links []string
	each(doc, func(n *html.Node) {
		if h, ok := attr(n, "href"); ok {
			if u, err := url.QueryUnescape(h); err == nil {
				h = u
			}
			links = append(links, h)
		}
	})
	each(doc, func(n *html.Node) {
		q, ok := attr(n, "data-record")
		if !ok {
			return
		}
		where, ok := parseRecord(q)
		if !ok {
			c.errorf("has a malformed data-record %q", q)
			return
		}
		for _, k := range slices.Sorted(maps.Keys(where)) {
			v := where.Get(k)
			if allowed, ok := types[k]; ok {
				// A refusal names the document it came in, whatever its
				// route: a format the page's handler read is a type too.
				if k == "format" && c.docFormat(v) {
					continue
				}
				if !slices.Contains(allowed, v) {
					c.errorf("data-record %q names the %s %q, none of %q", q, k, v, allowed)
				}
				continue
			}
			if !strings.Contains(visible, v) && !slices.ContainsFunc(links, func(h string) bool { return strings.Contains(h, v) }) {
				c.errorf("data-record %q names %s %q, which the page shows in no text and no link", q, k, v)
			}
		}
	})
}

// replaceWord drops each run of s that is word and that no letter or digit
// borders.
func replaceWord(s, word string) string {
	if word == "" {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, word)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := i + len(word)
		before, after := i > 0 && wordRune(s[i-1]), j < len(s) && wordRune(s[j])
		if before || after {
			b.WriteString(s[:j])
		} else {
			b.WriteString(s[:i] + " ")
		}
		s = s[j:]
	}
}

func wordRune(b byte) bool {
	return b < 0x80 && (unicode.IsLetter(rune(b)) || unicode.IsDigit(rune(b)))
}

func (c *wordCheck) digits(b wordBlock) {
	c.t.Helper()
	if b.node != nil && (b.node.Data == "title" || up(b.node, classed("crumbs")) != nil) {
		c.named("the title", b.text)
		return
	}
	if !strings.ContainsAny(b.text, "0123456789") {
		return
	}
	if s := c.strip(b.text); strings.ContainsAny(s, "0123456789") {
		c.errorf("writes a number outside template n: %q", b.text)
	}
}

// named judges the text of a title or a readable attribute: a run of it
// with a digit is a name or the whole text of an identifier of the page.
func (c *wordCheck) named(what, s string) {
	c.t.Helper()
	if !strings.ContainsAny(s, "0123456789") {
		return
	}
	for _, w := range strings.Fields(c.stripNames(s)) {
		w = strings.Trim(w, ".,:;()")
		if strings.ContainsAny(w, "0123456789") && !c.ids[w] {
			c.errorf("%s %q holds %q, which is no identifier the page shows", what, s, w)
		}
	}
}

func (c *wordCheck) attrs(doc *html.Node) {
	each(doc, func(n *html.Node) {
		for _, a := range n.Attr {
			readable := slices.Contains(readableAttrs, a.Key)
			if a.Key == "value" && n.Data == "input" {
				typ, _ := attr(n, "type")
				name, _ := attr(n, "name")
				readable = typ != "hidden" && typ != "radio" && typ != "checkbox" && (name != "find" || a.Val != c.find)
			}
			if readable {
				c.named("<"+n.Data+" "+a.Key+">", a.Val)
			}
		}
	})
}

// nones judges each sentence of nones in b, and fails a word of noneWords
// said in b's own words outside one.
func (c *wordCheck) nones(b wordBlock, say judge) {
	c.t.Helper()
	if b.inField {
		return
	}
	s := b.text
	for _, key := range slices.Sorted(func(yield func(string) bool) {
		for k := range ui.Nones {
			if !yield(k) {
				return
			}
		}
	}) {
		sentence := ui.Nones[key]
		if !strings.Contains(s, sentence) {
			continue
		}
		if say != nil && !say(key, b.node) {
			c.errorf("says %q, which the rows the reader may read do not bear out", sentence)
		}
		s = strings.ReplaceAll(s, sentence, " ")
	}
	if !noneWords.MatchString(s) {
		return
	}
	if m := noneWords.FindString(terms.ReplaceAllString(c.strip(s), " ")); m != "" {
		c.errorf("says %q outside func none: %q", m, b.text)
	}
}

// reports keeps what the render check reports.
type reports struct{ errs []string }

func (r *reports) Helper() {}

func (r *reports) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// TestRenderCheckCatchesInjectedCounts holds the render check to what it
// claims: a count written outside template n is caught in a quotation,
// in a readable attribute, as a message, and as a reference, and only the
// filter's own text, a given name, and the very field of the one record
// an element's data-record picks, in the documents the page's handler
// read, pass with a digit. A data-record that is missing, malformed, picks
// no record or more than one, or names a value the page does not show,
// fails. The cases marked review are those of the review of ui-v2-9.
func TestRenderCheckCatchesInjectedCounts(t *testing.T) {
	const message = "serial 3 of the state was written outside writ serve"
	served := `{"alerts":[{"kind":"outside-write","message":"` + message + `"},{"kind":"run-failed","run":"r1","message":"12345"},{"kind":"drifted","env":"prod","message":"2 changes outside OpenTofu"},{"kind":"drifted","env":"dev","message":"3 changes outside OpenTofu"}],"plans":[{"ref":"p1","author":{"display":"Ada 2"}},{"ref":"p2","author":{"display":"Bo 3"},"origin":{"pipeline":"12345"}}]}`
	for _, c := range []struct {
		name, path, main, served string
		names                    []string
		caught                   bool
	}{
		{"a quoted count", "/ui/", `<p>“999999 plans are applying”</p>`, served, nil, true},
		{"a quoted count in a title", "/ui/", `<p title="“999999 plans are applying”">Applying</p>`, served, nil, true},
		{"a quoted count beside the filter's text", "/ui/?find=web", `<p>Stacks match “web”. “999999 plans are applying”</p>`, served, nil, true},
		{"a count as a message", "/ui/", `<p><span class="msg" data-field="message">999999 plans are applying</span></p>`, served, nil, true},
		{"a count as a reference", "/ui/", `<p>By <span class="ref" data-field="author.display">999999</span></p>`, served, nil, true},
		{"part of a served message", "/ui/", `<p><span class="msg" data-record="kind=outside-write" data-field="message">serial 3 of the state</span></p>`, served, nil, true},
		{"a message with no document", "/ui/", `<p><span class="msg" data-record="kind=outside-write" data-field="message">` + message + `</span></p>`, "", nil, true},
		{"a served message that names no field", "/ui/", `<p><span class="msg" data-record="kind=outside-write">` + message + `</span></p>`, served, nil, true},
		{"a served message with no data-record", "/ui/", `<p><span class="msg" data-field="message">` + message + `</span></p>`, served, nil, true},
		{"a served message of another record", "/ui/", `<p><span class="msg" data-record="kind=drift-check-failed" data-field="message">` + message + `</span></p>`, served, nil, true},
		{"a served id as a message", "/ui/", `<p><a class="id" href="/ui/plans/p2">p2</a> <span class="msg" data-record="ref=p2" data-field="origin.pipeline">pipeline 12345</span></p>`, served, nil, true},
		{"a served id as a reference", "/ui/", `<p><a class="id" href="/ui/plans/p2">p2</a> <span class="ref" data-record="ref=p2" data-field="pipeline">pipeline 12345</span></p>`, served, nil, true},
		{"a served name of another record", "/ui/", `<p><a class="id" href="/ui/plans/p2">p2</a> by <span class="ref" data-record="ref=p2" data-field="author.display">Ada 2</span></p>`, served, nil, true},
		{"review: a numeric message as a count", "/ui/", `<p>Applying <a class="id" href="/ui/runs/r1">r1</a> <span class="msg" data-record="kind=run-failed&run=r1" data-field="message">12345</span> plans</p>`, served, nil, true},
		{"review: a missing record that would pick another author", "/ui/plans", `<p>By <span class="ref" data-field="author.display">Bo 3</span></p>`, served, nil, true},
		{"review: a record of another author", "/ui/plans", `<p><a class="id" href="/ui/plans/p1">p1</a> by <span class="ref" data-record="ref=p1" data-field="author.display">Bo 3</span></p>`, served, nil, true},
		{"review: a malformed record", "/ui/plans", `<p>By <span class="ref" data-record="ref=%ZZ" data-field="author.display">Bo 3</span></p>`, served, nil, true},
		{"review: a document only the footer names", "/ui/repos", `<p><span class="msg" data-record="kind=outside-write" data-field="message">` + message + `</span></p></main><footer class="foot"><code>GET /v1/not-rendered</code></footer><main>`, `{}`, nil, true},
		{"a record with an empty value", "/ui/", `<p><span class="msg" data-record="kind=outside-write&env=" data-field="message">` + message + `</span></p>`, served, nil, true},
		{"a record with a key twice", "/ui/", `<p><a class="id" href="/ui/plans/p1">p1</a> <a class="id" href="/ui/plans/p2">p2</a> <span class="ref" data-record="ref=p1&ref=p2" data-field="author.display">Ada 2</span></p>`, served, nil, true},
		{"a record that picks two", "/ui/", `<p><span class="msg" data-record="kind=drifted" data-field="message">2 changes outside OpenTofu</span></p>`, served, nil, true},
		{"a record that names a value the page does not show", "/ui/", `<p data-record="ref=p1">By Ada</p>`, served, nil, true},
		{"a record of no kind of alert", "/ui/", `<p data-record="kind=secret">Something</p>`, served, nil, true},
		{"review: a nested format as a document type", "/ui/", `<p><span class="msg" data-record="format=hidden-record-42" data-field="message">failure 42</span></p>`, `{"format":"writ.overview/v0","rows":[{"format":"hidden-record-42","message":"failure 42"}]}`, nil, true},
		{"review: a fixed phrase in a title", "/ui/", `<p title="the newest 200 plans are applying">Applying</p>`, served, nil, true},
		{"review: a fixed phrase in an aria-label", "/ui/", `<p aria-label="24 hours">Runs</p>`, served, nil, true},
		{"the filter's own text", "/ui/?find=web2", `<p>Stacks match “web2”.</p>`, served, nil, false},
		{"a served message of its record", "/ui/", `<p><span class="msg" data-record="kind=outside-write" data-field="message">` + message + `</span></p>`, served, nil, false},
		{"a served message of its record among two of a kind", "/ui/", `<p>prod <span class="msg" data-record="kind=drifted&env=prod" data-field="message">2 changes outside OpenTofu</span></p>`, served, nil, false},
		{"a served name of its record", "/ui/", `<p data-record="ref=p1"><a class="id" href="/ui/plans/p1">p1</a> by <span class="ref" data-field="author.display">Ada 2</span></p>`, served, nil, false},
		{"a given name", "/ui/", `<p>By <span class="ref">web2</span></p>`, served, []string{"web2"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc, err := html.Parse(strings.NewReader(`<!doctype html><html><head><title>Overview</title></head><body><main id="main">` + c.main + `</main></body></html>`))
			if err != nil {
				t.Fatal(err)
			}
			var r reports
			checkWords(&r, c.path, doc, [][]byte{[]byte(c.served)}, c.names, nil)
			if caught := len(r.errs) > 0; caught != c.caught {
				t.Errorf("caught %v, want %v: %q", caught, c.caught, r.errs)
			}
		})
	}
}

// TestRenderCheckVouchesByField holds the check to the field a served
// element renders, on a real overview: its plan came from pipeline
// 12345, so the overview holds that string, but in no message or name
// field, and a count of 12345 set as a message or a reference is caught.
func TestRenderCheckVouchesByField(t *testing.T) {
	f := newFixtureOn(t, controltest.SQLite, "")
	f.plan(map[string]any{"pipeline_id": "12345"})
	b := f.browser()
	b.signIn("/ui/")
	if !strings.Contains(string(b.read("/ui/?as=json")), `"12345"`) {
		t.Fatal("the overview holds no string 12345, so this test would prove nothing")
	}
	real := b.get("/ui/")
	for _, c := range []struct {
		name, main string
		caught     bool
	}{
		{"the page as served", "", false},
		{"a count as a message", `<p>Applying <span class="msg">12345</span> plans</p>`, true},
		{"a count as a reference", `<p>Applying <span class="ref">12345</span> plans</p>`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := strings.Replace(real.body, `<main id="main" tabindex="-1">`, `<main id="main" tabindex="-1">`+c.main, 1)
			doc, err := html.Parse(strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			var r reports
			checkWords(&r, "/ui/", doc, real.served, nil, nil)
			if caught := len(r.errs) > 0; caught != c.caught {
				t.Errorf("caught %v, want %v: %q", caught, c.caught, r.errs)
			}
		})
	}
}
