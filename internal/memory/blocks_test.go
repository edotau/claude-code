package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendBlocksPreservesBytes(t *testing.T) {
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
