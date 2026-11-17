package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/mattn/go-isatty"
)

func escapeHuman(s string, layout bool) string {
	var b strings.Builder
	for _, r := range s {
		if (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) && !(layout && r == '\n') {
			if r <= 0xffff {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// out is human output. Every byte written to it goes through escapeHuman,
// so no value can move the cursor or recolor the terminal. Color comes only
// from line, from the fixed styles below, and only when color is set.
type out struct {
	w     io.Writer
	color bool
}

// humanOut returns w as human output. A w that is already human output
// keeps its settings; any other writer gets plain text.
func humanOut(w io.Writer) out {
	if o, ok := w.(out); ok {
		return o
	}
	return out{w: w}
}

func (o out) Write(p []byte) (int, error) {
	if _, err := io.WriteString(o.w, escapeHuman(string(p), true)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// style is an SGR parameter list. The zero style prints plain text.
type style string

const (
	plain      style = ""
	bold       style = "1"
	dim        style = "2"
	red        style = "31"
	green      style = "32"
	yellow     style = "33"
	cyan       style = "36"
	boldRed    style = "1;31"
	boldYellow style = "1;33"
	boldGreen  style = "1;32"
)

// seg is a piece of a line and its style.
type seg struct {
	st   style
	text string
}

// txt is a plain piece of a line.
func txt(s string) seg { return seg{text: s} }

// of styles s.
func (st style) of(s string) seg { return seg{st: st, text: s} }

// line writes segs and a newline as one write, each segment escaped, and
// styled when color is on.
func (o out) line(segs ...seg) {
	var b strings.Builder
	for _, s := range segs {
		text := escapeHuman(s.text, false)
		if o.color && s.st != plain && text != "" {
			b.WriteString("\x1b[" + string(s.st) + "m" + text + "\x1b[0m")
			continue
		}
		b.WriteString(text)
	}
	b.WriteByte('\n')
	io.WriteString(o.w, b.String())
}

// ColorFor reports whether human output to w should be colored, by the
// process environment. See wantColor.
func ColorFor(w io.Writer) bool { return wantColor(w, os.LookupEnv) }

// wantColor reports whether output to w should be colored: w is a terminal,
// NO_COLOR is unset or empty (https://no-color.org), and TERM is not dumb.
func wantColor(w io.Writer, lookupEnv func(string) (string, bool)) bool {
	if !isTerminal(w) {
		return false
	}
	if v, ok := lookupEnv("NO_COLOR"); ok && v != "" {
		return false
	}
	term, _ := lookupEnv("TERM")
	return term != "dumb"
}

// isTerminal reports whether w is a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && (isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd()))
}

func (a *App) humanOutput() out {
	return out{w: a.opts.Stdout, color: a.opts.Color}
}

type terminalError struct{ error }

func (e terminalError) Error() string { return escapeHuman(e.error.Error(), false) }
func (e terminalError) Unwrap() error { return e.error }

func escapeError(err *error, asJSON bool) {
	if !asJSON && *err != nil && escapeHuman((*err).Error(), false) != (*err).Error() {
		*err = terminalError{*err}
	}
}
