package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// The protocol between Decide and an evaluation process.
//
// The parent writes one line, a childRequest, and then the input document
// to the child's stdin. The child writes one line for each set, a result
// with the set's index and either a decision or an error, and then one
// line {"done": N}, where N counts the result lines. The parent accepts
// the output only when it is all of that, each line a complete object
// whose keys are exactly the protocol's, in their case and each once,
// every set answered once, nothing after the done line, and the process
// exits 0. Anything else makes every set an error.

// childRequest is the first line the parent sends: the memory limit and
// the sets.
type childRequest struct {
	Memory int64      `json:"memory"`
	Sets   []childSet `json:"sets"`
}

type childSet struct {
	Name  string            `json:"name"`
	Files map[string]string `json:"files"`
}

// childLine is one line the child writes: a result or the done line.
type childLine struct {
	Set      *int           `json:"set,omitempty"`
	Decision *childDecision `json:"decision,omitempty"`
	Error    *string        `json:"error,omitempty"`
	Done     *int           `json:"done,omitempty"`
}

// childDecision is a set's messages. Every field is present on the wire.
type childDecision struct {
	Messages *[]childMessage `json:"messages"`
}

type childMessage struct {
	Rule    *string `json:"rule"`
	Message *string `json:"message"`
}

// setResult is what the parent keeps of one set: its messages, or an
// error.
type setResult struct {
	messages []message
	err      string
}

// message is one message of a rule Writ reads.
type message struct {
	rule, text string
}

// outcome is what a message does at a mandatory set's level.
func (m message) outcome() Outcome {
	if denyRule.MatchString(m.rule) {
		return Deny
	}
	return Warn
}

func resultLine(i int, r setResult) childLine {
	if r.err != "" {
		return childLine{Set: &i, Error: &r.err}
	}
	msgs := make([]childMessage, 0, len(r.messages))
	for _, m := range r.messages {
		msgs = append(msgs, childMessage{Rule: &m.rule, Message: &m.text})
	}
	return childLine{Set: &i, Decision: &childDecision{Messages: &msgs}}
}

// readOutput checks a child's whole output against the protocol for n sets
// and returns each set's result.
func readOutput(out []byte, n int) ([]setResult, error) {
	if len(out) == 0 || out[len(out)-1] != '\n' {
		return nil, errors.New("the output does not end with a complete line")
	}
	lines := bytes.Split(out[:len(out)-1], []byte("\n"))
	if len(lines) != n+1 {
		return nil, fmt.Errorf("the output has %d lines for %d sets", len(lines), n)
	}
	results := make([]setResult, n)
	seen := make([]bool, n)
	for i, raw := range lines {
		if err := strictKeys(raw); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		var l childLine
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&l); err != nil {
			return nil, fmt.Errorf("line %d is not a result: %w", i+1, err)
		}
		if dec.More() {
			return nil, fmt.Errorf("line %d has bytes after its object", i+1)
		}
		if i == n {
			if l.Done == nil || l.Set != nil || l.Decision != nil || l.Error != nil || *l.Done != n {
				return nil, fmt.Errorf("the last line is not {\"done\": %d}", n)
			}
			break
		}
		if l.Done != nil || l.Set == nil || (l.Decision == nil) == (l.Error == nil) {
			return nil, fmt.Errorf("line %d is not a set's result with one decision or one error", i+1)
		}
		s := *l.Set
		if s < 0 || s >= n || seen[s] {
			return nil, fmt.Errorf("line %d answers set %d again or out of range", i+1, s)
		}
		seen[s] = true
		if l.Error != nil {
			if *l.Error == "" {
				return nil, fmt.Errorf("line %d has an empty error", i+1)
			}
			results[s].err = *l.Error
			continue
		}
		if l.Decision.Messages == nil {
			return nil, fmt.Errorf("line %d has a decision without messages", i+1)
		}
		msgs := []message{}
		for _, m := range *l.Decision.Messages {
			if m.Rule == nil || m.Message == nil || !(denyRule.MatchString(*m.Rule) || warnRule.MatchString(*m.Rule)) {
				return nil, fmt.Errorf("line %d has a message without a rule Writ reads or without text", i+1)
			}
			msgs = append(msgs, message{rule: *m.Rule, text: *m.Message})
		}
		results[s] = setResult{messages: msgs}
	}
	return results, nil
}

// lineKeys are the keys each object of a line may have, by the key that
// holds the object, "" for the line itself. Go's decoder matches a key
// without regard to case and keeps the last of two equal keys, so
// strictKeys checks the keys first.
var lineKeys = map[string][]string{
	"":           {"set", "decision", "error", "done"},
	"decision":   {"messages"},
	"messages[]": {"rule", "message"},
}

// strictKeys refuses a line with a key that is not exactly one of
// lineKeys, with a key twice in one object, or with a null under a key or
// in a list. Go's decoder reads a null as an absent field.
func strictKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return checkValue(dec, "")
}

func checkValue(dec *json.Decoder, holder string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil && holder != "" {
		return fmt.Errorf("%q is null", holder)
	}
	switch tok {
	case json.Delim('{'):
		allowed, ok := lineKeys[holder]
		if !ok {
			return fmt.Errorf("an object under %q", holder)
		}
		seen := map[string]bool{}
		for dec.More() {
			t, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := t.(string)
			if !slices.Contains(allowed, key) {
				return fmt.Errorf("the key %q is not one of %v", key, allowed)
			}
			if seen[key] {
				return fmt.Errorf("the key %q appears twice", key)
			}
			seen[key] = true
			if err := checkValue(dec, key); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case json.Delim('['):
		for dec.More() {
			if err := checkValue(dec, holder+"[]"); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	}
	return nil
}
