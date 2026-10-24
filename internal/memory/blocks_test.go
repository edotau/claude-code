package memory

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// splitHeadingBlocks edge cases: a "## " line fenced in ``` is never a heading boundary; a ~~~ line
// nested inside an open ``` fence closes it early (today's hand-rolled fenceToggle shares one bit across
// both fence chars — this pins that quirk; true nested-fence tracking would be a named correctness fix);
// an unclosed fence runs to EOF; CRLF line endings keep their "\r"; a missing trailing newline still
// yields the last block; duplicate headings are kept as separate blocks, nothing dedupes on text.
func TestSplitHeadingBlocksEdgeCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		in         string
		wantHeader string
		want       []string
	}{
		{
			name:       "fence aware",
			in:         "header\n\n## real\nbody\n```\n## fake\n```\nmore\n\n## real2\nbody2\n",
			wantHeader: "header",
			want:       []string{"## real\nbody\n```\n## fake\n```\nmore", "## real2\nbody2"},
		},
		{
			name:       "nested fence toggles early",
			in:         "h\n\n## a\n```\n~~~\n## nope\n~~~\nstill fence\n",
			wantHeader: "h",
			want:       []string{"## a\n```\n~~~", "## nope\n~~~\nstill fence"},
		},
		{
			name:       "unclosed fence",
			in:         "h\n\n## a\n```\n## nope\n",
			wantHeader: "h",
			want:       []string{"## a\n```\n## nope"},
		},
		{
			name:       "CRLF keeps carriage returns",
			in:         "h\r\n\r\n## a\r\nbody\r\n",
			wantHeader: "h\r\n\r",
			want:       []string{"## a\r\nbody\r"},
		},
		{
			name:       "no trailing newline",
			in:         "h\n\n## a\nbody",
			wantHeader: "h",
			want:       []string{"## a\nbody"},
		},
		{
			name:       "duplicate headings",
			in:         "h\n\n## dup\nfirst\n\n## dup\nsecond\n",
			wantHeader: "h",
			want:       []string{"## dup\nfirst", "## dup\nsecond"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			header, blocks := splitHeadingBlocks(tt.in)
			if header != tt.wantHeader {
				t.Errorf("header = %q, want %q", header, tt.wantHeader)
			}
			if !reflect.DeepEqual(blocks, tt.want) {
				t.Errorf("blocks = %#v, want %#v", blocks, tt.want)
			}
		})
	}
}

// rejoin(splitOn(x)) == x for a well-formed, trailing-newline-terminated file (fences included).
func TestRejoinSplitHeadingBlocksRoundTrip(t *testing.T) {
	t.Parallel()
	in := "header\n\n## real\nbody\n```\n## fake\n```\nmore\n\n## real2\nbody2\n"
	header, blocks := splitHeadingBlocks(in)
	if got := rejoin(header, blocks); got != in {
		t.Errorf("rejoin(splitHeadingBlocks(x)) = %q, want %q", got, in)
	}
}

// fenceToggle fires on an indented ``` fence (with a language tag) and on a bare ~~~ fence.
func TestFenceToggleVariants(t *testing.T) {
	t.Parallel()
	if !fenceToggle("   ```go") {
		t.Error("an indented ``` fence with a language tag must toggle")
	}
	if !fenceToggle("~~~") {
		t.Error("a bare ~~~ fence must toggle")
	}
	if fenceToggle("not a fence") {
		t.Error("ordinary text must not toggle")
	}
}

// NewestHeadingBlocks: n below the count truncates to the newest n, n above returns everything, a file
// with no trailing newline still yields its last block, and a missing file returns nil (not empty).
func TestNewestHeadingBlocksEdgeCases(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "decisionLog.md")
	body := "h\n\n## one\nfirst\n```\n## two fake\n```\n\n## two\nsecond\n\n## three\nthird\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	want2 := []string{"## one\nfirst\n```\n## two fake\n```", "## two\nsecond"}
	if got := NewestHeadingBlocks(p, 2); !reflect.DeepEqual(got, want2) {
		t.Errorf("NewestHeadingBlocks(n=2) = %#v, want %#v", got, want2)
	}
	wantAll := []string{"## one\nfirst\n```\n## two fake\n```", "## two\nsecond", "## three\nthird"}
	if got := NewestHeadingBlocks(p, 100); !reflect.DeepEqual(got, wantAll) {
		t.Errorf("NewestHeadingBlocks(n>count) = %#v, want %#v", got, wantAll)
	}

	p2 := filepath.Join(dir, "dl2.md")
	if err := os.WriteFile(p2, []byte("h\n\n## only\nbody no nl"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantNoNL := []string{"## only\nbody no nl"}
	if got := NewestHeadingBlocks(p2, 5); !reflect.DeepEqual(got, wantNoNL) {
		t.Errorf("NewestHeadingBlocks(no trailing newline) = %#v, want %#v", got, wantNoNL)
	}

	if got := NewestHeadingBlocks(filepath.Join(dir, "absent.md"), 5); got != nil {
		t.Errorf("NewestHeadingBlocks(missing file) = %#v, want nil", got)
	}
}

func TestAppendBlocksPreservesBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		existing string
		create   bool
		blocks   []string
		want     string
	}{
		{name: "missing", blocks: []string{"- first"}, want: "# topic (archived)\n\n- first\n"},
		{name: "empty", create: true, want: "# topic (archived)\n"},
		{name: "whitespace", create: true, existing: " \t\n", blocks: []string{""}, want: "# topic (archived)\n\n\n"},
		{name: "header without newline", create: true, existing: "# Existing", blocks: []string{"- first", "- second"}, want: "# Existing\n\n- first\n\n- second\n"},
		{name: "header with newlines", create: true, existing: "# Existing\n\n", blocks: []string{"- first\n", "", "## Heading\nbody\n\n"}, want: "# Existing\n\n\n- first\n\n\n\n\n## Heading\nbody\n\n\n"},
		{name: "no blocks", create: true, existing: "# Existing\n\n", want: "# Existing\n\n"},
		{name: "no blocks adds newline", create: true, existing: "# Existing", want: "# Existing\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "topic.md")
			if tt.create {
				write(t, path, tt.existing)
			}
			if err := appendBlocks(path, tt.blocks); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("appendBlocks bytes = %q, want %q", got, tt.want)
			}
		})
	}
}
