package ui

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"text/template/parse"
)

// Routes are the patterns of every route of the control UI.
func (u *UI) Routes() []string {
	var out []string
	for _, rt := range u.routes() {
		out = append(out, rt.pattern)
	}
	return out
}

// TraceTemplates makes every template of u that renders anything call
// seen with its name when it renders, and returns their names. Pages, the
// parts they include, and a {{define}} named like a file count alike, so
// no template, however it is named or wherever it is used, escapes a check
// that asks for each to render. A file of templates holds only {{define}}
// blocks and whitespace: it fails with content outside them, which no
// page would render. Call it before u serves a request.
func (u *UI) TraceTemplates(seen func(name string)) ([]string, error) {
	files, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, t := range u.tmpl.Templates() {
		if t.Tree == nil || blank(t.Tree.Root) {
			continue
		}
		if slices.Contains(files, "templates/"+t.Name()) {
			return nil, fmt.Errorf("templates/%s holds content outside a {{define}}, which no page renders: %.80q", t.Name(), strings.TrimSpace(t.Tree.Root.String()))
		}
		names = append(names, t.Name())
	}
	slices.Sort(names)
	u.tmpl.Funcs(template.FuncMap{"traceTemplate": func(name string) string { seen(name); return "" }})
	for _, name := range names {
		inner := name + " untraced"
		if _, err := u.tmpl.AddParseTree(inner, u.tmpl.Lookup(name).Tree.Copy()); err != nil {
			return nil, err
		}
		if _, err := u.tmpl.New(name).Parse(fmt.Sprintf(`{{traceTemplate %q}}{{template %q .}}`, name, inner)); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// TraceFetches makes u call seen with each answer writ serve gives a
// page's handler, and the page's request, so the render check reads the
// documents the page read and no other. Call it before u serves.
func (u *UI) TraceFetches(seen func(r *http.Request, body []byte)) { u.fetched = seen }

// blank reports whether root is whitespace and nothing else.
func blank(root *parse.ListNode) bool {
	for _, n := range root.Nodes {
		if t, ok := n.(*parse.TextNode); !ok || strings.TrimSpace(string(t.Text)) != "" {
			return false
		}
	}
	return true
}

// Nones are the sentences template func none says, by key.
var Nones = nones

// TemplateText is what template name writes in its own words: its text,
// with each template it calls written in place and each action as \x00.
func TemplateText(name string) (string, error) {
	t, err := parseTemplates(&UI{})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var walk func(n parse.Node, in []string) error
	walk = func(n parse.Node, in []string) error {
		var lists []*parse.ListNode
		switch n := n.(type) {
		case *parse.ListNode:
			lists = append(lists, n)
		case *parse.TextNode:
			b.Write(n.Text)
		case *parse.ActionNode:
			b.WriteByte(0)
		case *parse.IfNode:
			lists = append(lists, n.List, n.ElseList)
		case *parse.RangeNode:
			lists = append(lists, n.List, n.ElseList)
		case *parse.WithNode:
			lists = append(lists, n.List, n.ElseList)
		case *parse.TemplateNode:
			called := t.Lookup(n.Name)
			if called == nil || slices.Contains(in, n.Name) {
				return fmt.Errorf("template %q calls %q, which is missing or calls itself", name, n.Name)
			}
			return walk(called.Tree.Root, append(in, n.Name))
		}
		for _, l := range lists {
			if l == nil {
				continue
			}
			for _, c := range l.Nodes {
				if err := walk(c, in); err != nil {
					return err
				}
			}
		}
		return nil
	}
	root := t.Lookup(name)
	if root == nil {
		return "", fmt.Errorf("no template %q", name)
	}
	err = walk(root.Tree.Root, []string{name})
	return b.String(), err
}
