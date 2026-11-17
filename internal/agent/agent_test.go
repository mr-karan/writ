package agent

import (
	"bytes"
	"errors"
	"os"
	"path"
	"strings"
	"testing"
)

func TestMerge(t *testing.T) {
	section := string(Section())
	tests := []struct {
		name     string
		existing string
		want     string
		change   Change
		err      error
	}{
		{name: "no file", existing: "", want: section, change: Appended},
		{name: "blank lines are kept", existing: "\n\n", want: "\n\n" + section, change: Appended},
		{name: "whitespace is kept", existing: " \t\n", want: " \t\n\n" + section, change: Appended},
		{name: "whitespace with no final newline is kept", existing: "  ", want: "  \n\n" + section, change: Appended},
		{name: "appends after a final newline", existing: "# Rules\n\nBe brief.\n", want: "# Rules\n\nBe brief.\n\n" + section, change: Appended},
		{name: "appends after a blank line", existing: "# Rules\n\n", want: "# Rules\n\n" + section, change: Appended},
		{name: "appends with no final newline", existing: "# Rules", want: "# Rules\n\n" + section, change: Appended},
		{name: "already current", existing: "# Rules\n\n" + section + "\nMore.\n", want: "# Rules\n\n" + section + "\nMore.\n", change: Unchanged},
		{
			name:     "replaces an old section in place",
			existing: "# Rules\n" + beginMarker + "\nold text\n" + endMarker + "\nAfter.\n",
			want:     "# Rules\n" + section + "After.\n",
			change:   Replaced,
		},
		{
			name:     "replaces a section at the end with no final newline",
			existing: "# Rules\n" + beginMarker + "\nold\n" + endMarker,
			want:     "# Rules\n" + section,
			change:   Replaced,
		},
		{
			name:     "keeps CRLF text outside the section",
			existing: "# Rules\r\n" + beginMarker + "\r\nold\r\n" + endMarker + "\r\nAfter.\r\n",
			want:     "# Rules\r\n" + section + "After.\r\n",
			change:   Replaced,
		},
		{name: "opening marker alone", existing: "a\n" + beginMarker + "\nb\n", err: ErrBrokenSection},
		{name: "closing marker alone", existing: "a\n" + endMarker + "\n", err: ErrBrokenSection},
		{name: "closing before opening", existing: endMarker + "\n" + beginMarker + "\n", err: ErrBrokenSection},
		{name: "two sections", existing: section + section, err: ErrBrokenSection},
		{name: "a marker inside a line is text", existing: "see `" + beginMarker + "`\n", want: "see `" + beginMarker + "`\n\n" + section, change: Appended},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, change, err := Merge([]byte(tt.existing))
			if !errors.Is(err, tt.err) {
				t.Fatalf("err %v, want %v", err, tt.err)
			}
			if tt.err != nil {
				return
			}
			if string(got) != tt.want || change != tt.change {
				t.Fatalf("got %v\n%q\nwant %v\n%q", change, got, tt.change, tt.want)
			}
			if change == Appended && !strings.HasPrefix(string(got), tt.existing) {
				t.Fatalf("appending changed the bytes before the section: %q", got)
			}
			again, change, err := Merge(got)
			if err != nil || !bytes.Equal(again, got) || change != Unchanged {
				t.Fatalf("merging the result again changed it: %v\n%q", err, again)
			}
		})
	}
}

// The skill's reference is the docs page, byte for byte, so the skill an
// agent loads says what the site says.
func TestSkillCopiesTheDocs(t *testing.T) {
	for name, doc := range map[string]string{"reference.md": "agent-reference.md"} {
		want, err := os.ReadFile(path.Join("..", "..", "docs", doc))
		if err != nil {
			t.Fatal(err)
		}
		got, err := skill.ReadFile(path.Join("skill", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("internal/agent/skill/%s differs from docs/%s: run cp docs/%s internal/agent/skill/%s", name, doc, doc, name)
		}
	}
}

func TestSkillFiles(t *testing.T) {
	var paths []string
	for _, f := range SkillFiles(SkillDir) {
		paths = append(paths, f.Path)
		if len(f.Text) == 0 {
			t.Errorf("%s is empty", f.Path)
		}
	}
	want := SkillDir + "/SKILL.md " + SkillDir + "/reference.md"
	if got := strings.Join(paths, " "); got != want {
		t.Fatalf("files %s, want %s", got, want)
	}
	text := string(SkillFiles(SkillDir)[0].Text)
	if !strings.HasPrefix(text, "---\nname: writ\ndescription: ") || !strings.Contains(text, "\n---\n") {
		t.Fatalf("SKILL.md does not start with the frontmatter agents read:\n%.200s", text)
	}
}
