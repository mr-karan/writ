package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Bound returns a tool result's text within MaxOutput bytes and
// MaxDiagnostics diagnostics. A JSON object stays valid JSON: each
// "diagnostics" array in it keeps its first MaxDiagnostics, with the count
// dropped in "omitted_diagnostics" beside it, and a document still too
// long is replaced by one that keeps its format and its ok when they have
// their known shapes, its policy decision, and the diagnostics that fit,
// with "truncated" set. Other text keeps its first
// MaxDiagnostics diagnostics, then is cut at the last line that fits with
// a notice that says so.
func Bound(text string) string {
	if doc, ok := boundDocument(text); ok {
		return doc
	}
	return boundText(text)
}

// boundDocument bounds text that is one JSON object, and reports false for
// any other text.
func boundDocument(text string) (string, bool) {
	trimmed := bytes.TrimSpace([]byte(text))
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return "", false
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, capDiagnostics(trimmed)); err != nil {
		return "", false
	}
	if buf.Len() <= MaxOutput {
		return buf.String(), true
	}
	return truncatedDocument(buf.Bytes(), len(text)), true
}

// capDiagnostics returns the JSON value raw with every "diagnostics" array
// in it, at any depth, cut to MaxDiagnostics. Objects keep their key order.
func capDiagnostics(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return raw
	}
	switch raw[0] {
	case '[':
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil {
			return raw
		}
		for i := range items {
			items[i] = capDiagnostics(items[i])
		}
		return joinArray(items)
	case '{':
		fields, ok := objectFields(raw)
		if !ok {
			return raw
		}
		var out []field
		for _, f := range fields {
			var items []json.RawMessage
			if f.key == "diagnostics" && json.Unmarshal(f.value, &items) == nil && len(items) > MaxDiagnostics {
				kept := items[:MaxDiagnostics]
				for i := range kept {
					kept[i] = capDiagnostics(kept[i])
				}
				out = append(out, field{f.key, joinArray(kept)})
				out = append(out, field{"omitted_diagnostics", json.RawMessage(fmt.Sprint(len(items) - MaxDiagnostics))})
				continue
			}
			out = append(out, field{f.key, capDiagnostics(f.value)})
		}
		return joinObject(out)
	}
	return raw
}

type field struct {
	key   string
	value json.RawMessage
}

// objectFields returns the fields of the JSON object raw in their order.
func objectFields(raw json.RawMessage) ([]field, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var fields []field
	for dec.More() {
		tok, err := dec.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return nil, false
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, false
		}
		fields = append(fields, field{key, value})
	}
	return fields, true
}

func joinArray(items []json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(item)
	}
	b.WriteByte(']')
	return b.Bytes()
}

func joinObject(fields []field) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(jsonString(f.key))
		b.WriteByte(':')
		b.Write(f.value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// jsonString encodes s as a JSON string without escaping <, >, and &, so a
// text such as a <-> b reads as itself.
func jsonString(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		panic(fmt.Sprintf("encode a string: %v", err))
	}
	return bytes.TrimRight(b.Bytes(), "\n")
}

// documentFormat is the shape of a "format" field that a truncated
// document keeps, such as writ.remote-plan/v0. Any other format, which a control
// plane may send, is dropped, so the head of a truncated document is bounded.
var documentFormat = regexp.MustCompile(`^writ\.[a-z0-9.-]{1,48}/v[0-9]{1,4}$`)

// truncatedDocument stands in for a compact document past MaxOutput: its
// format and ok when they have their known shapes, its policy decision
// with as many of its first results as fit in half of MaxOutput, as many
// of its first diagnostics as fit, and a message. It is within MaxOutput.
func truncatedDocument(doc []byte, size int) string {
	fields, _ := objectFields(doc)
	var head []field
	var diags []json.RawMessage
	for _, f := range fields {
		switch {
		case f.key == "policy":
			if p, ok := boundPolicy(f.value, MaxOutput/2); ok {
				head = append(head, field{f.key, p})
			}
		case f.key == "format":
			var format string
			if json.Unmarshal(f.value, &format) == nil && documentFormat.MatchString(format) {
				head = append(head, field{f.key, jsonString(format)})
			}
		case f.key == "ok" && (string(f.value) == "true" || string(f.value) == "false"):
			head = append(head, f)
		case f.key == "diagnostics":
			json.Unmarshal(f.value, &diags)
		}
	}
	head = append(head,
		field{"truncated", json.RawMessage("true")},
		field{"message", jsonString(fmt.Sprintf("writ mcp left out this result: it is %d bytes, and a result holds at most %d; narrow the request, or run the command in a shell", size, MaxOutput))},
	)
	withDiags := func(kept int) json.RawMessage {
		return joinObject(slices.Concat(head, []field{{"diagnostics", joinArray(diags[:kept])}, {"omitted_diagnostics", json.RawMessage(fmt.Sprint(len(diags) - kept))}}))
	}
	kept := 0
	for kept < len(diags) && kept < MaxDiagnostics && len(withDiags(kept+1)) <= MaxOutput {
		kept++
	}
	if out := withDiags(kept); len(out) <= MaxOutput {
		return string(out)
	}
	// The head is bounded, so this is not reached; the cap holds even so.
	return string(joinObject(head[len(head)-2:]))
}

// decisionWord is the shape of a policy decision's point and outcome that
// a truncated document keeps.
var decisionWord = regexp.MustCompile(`^"[a-z]{1,16}"$`)

// boundPolicy returns a writ.policy-decision/v1 within limit bytes: its
// format, point, and outcome, as many of its first results as fit, and the
// count of the rest in "omitted_results". It reports false for any other
// value, so an agent never reads a verdict it cannot trust.
func boundPolicy(raw json.RawMessage, limit int) (json.RawMessage, bool) {
	fields, ok := objectFields(raw)
	if !ok {
		return nil, false
	}
	var kept []field
	var results []json.RawMessage
	for _, f := range fields {
		switch {
		case f.key == "format" && string(f.value) == `"writ.policy-decision/v1"`,
			(f.key == "point" || f.key == "outcome") && decisionWord.Match(f.value):
			kept = append(kept, f)
		case f.key == "results":
			if json.Unmarshal(f.value, &results) != nil {
				return nil, false
			}
		}
	}
	if len(kept) != 3 {
		return nil, false
	}
	// Results arrive in config order; a deny is what the agent must fix, so
	// denies go first, then warnings, then the rest, each in stored order.
	slices.SortStableFunc(results, func(a, b json.RawMessage) int { return resultRank(a) - resultRank(b) })
	// The omitted count has at most as many digits as the results' count.
	size := len(joinObject(append(kept, field{"results", json.RawMessage("[]")}, field{"omitted_results", json.RawMessage(fmt.Sprint(len(results)))})))
	n := 0
	for n < len(results) && size+len(results[n])+1 <= limit {
		size += len(results[n]) + 1
		n++
	}
	return joinObject(append(kept, field{"results", joinArray(results[:n])}, field{"omitted_results", json.RawMessage(fmt.Sprint(len(results) - n))})), true
}

// resultRank orders a policy result for truncation: deny, then warn, then
// anything else, which includes a result whose outcome cannot be read.
func resultRank(r json.RawMessage) int {
	var v struct {
		Outcome string `json:"outcome"`
	}
	if json.Unmarshal(r, &v) != nil {
		return 0
	}
	switch v.Outcome {
	case "deny":
		return 0
	case "warn":
		return 1
	}
	return 2
}

// diagnosticLine starts a diagnostic in Writ's text output: error[usage]
// main.tf:3:5 from a command, or error: usage: ... from a control plane.
var diagnosticLine = regexp.MustCompile(`^(error|warning)(\[[a-z0-9-]+\]|: [a-z0-9-]+: )`)

// boundText keeps the first MaxDiagnostics diagnostics of text, each its
// head line and the indented lines under it, then cuts the text past
// MaxOutput at the last line that fits.
func boundText(text string) string {
	var b strings.Builder
	seen, skipping := 0, false
	for line := range strings.Lines(text) {
		switch {
		case diagnosticLine.MatchString(line):
			seen++
			skipping = seen > MaxDiagnostics
		case skipping && strings.HasPrefix(line, "  "):
		default:
			skipping = false
		}
		if !skipping {
			b.WriteString(line)
		}
	}
	if seen > MaxDiagnostics {
		fmt.Fprintf(&b, "[writ mcp left out %d diagnostics after the first %d; fix these, then run the tool again]\n", seen-MaxDiagnostics, MaxDiagnostics)
	}
	text = b.String()
	if len(text) <= MaxOutput {
		return text
	}
	notice := func(kept int) string {
		return fmt.Sprintf("\n[writ mcp cut this result at %d of %d bytes; narrow the request, or run the command in a shell]\n", kept, len(text))
	}
	// The notice for any cut is no longer than the one for a cut at
	// MaxOutput, so reserving that much keeps the result within it.
	cut := text[:MaxOutput-len(notice(MaxOutput))]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i+1]
	}
	// A cut with no newline may end inside a rune.
	for range utf8.UTFMax - 1 {
		if r, size := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError || size != 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut + notice(len(cut))
}
