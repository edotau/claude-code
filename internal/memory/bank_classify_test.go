package memory

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFrontmatterValueTolerantFencesAndRawValues: frontmatterValue's manual scan is whitespace-tolerant
// on the fence line and never coerces a scalar to another YAML type (the two regressions this reverts).
func TestFrontmatterValueTolerantFencesAndRawValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, raw, key, want string
	}{
		{"trailing-space fence", "---  \nname: a\n---  \nbody", "name", "a"},
		{"indented fence", "  ---\nname: a\n  ---\nbody", "name", "a"},
		{"no final newline", "---\nname: a\n---", "name", "a"},
		{"name: yes stays raw text, not a bool", "---\nname: yes\n---\nbody", "name", "yes"},
		{"description: [draft] stays raw text, not a list", "---\ndescription: [draft]\n---\nbody", "description", "[draft]"},
		{"name: 007 stays raw text, not a number", "---\nname: 007\n---\nbody", "name", "007"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := frontmatterValue(tc.raw, tc.key); got != tc.want {
				t.Errorf("frontmatterValue(%q, %q) = %q, want %q", tc.raw, tc.key, got, tc.want)
			}
		})
	}
}

// TestClassifyMemoryFile tables every name shape a native store dir can hold.
func TestClassifyMemoryFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range bankFiles {
		write(t, filepath.Join(dir, n), "bank body\n")
	}
	for _, n := range []string{nativeIndexFile, nativeSearchableFile, nativeArchiveIndexFile, catalogFile} {
		write(t, filepath.Join(dir, n), "reserved body\n")
	}
	write(t, filepath.Join(dir, legacyOriginFile), "{}\n") // origin now lives in the vault; the on-disk file is an unknown dotfile
	write(t, filepath.Join(dir, "notes.md"), "---\nname: notes\nharness_kind: bank\n---\n\nbody\n")
	write(t, filepath.Join(dir, "topic.md"), "---\nname: topic\n---\n\nbody\n")
	write(t, filepath.Join(dir, "real.md"), "---\nname: real\n---\n\nbody\n")
	if err := os.Symlink(filepath.Join(dir, "real.md"), filepath.Join(dir, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "subtree.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := map[string]fileKind{
		nativeIndexFile:        kindNativeIndex,
		nativeSearchableFile:   kindSearchable,
		nativeArchiveIndexFile: kindArchiveIndex,
		catalogFile:            kindCatalog,
		legacyOriginFile:       kindOther,
		"notes.md":             kindBank,
		"topic.md":             kindTopic,
		"link.md":              kindOther,
		"subtree.md":           kindOther,
		"../x":                 kindOther,
		"a/b.md":               kindOther,
		"":                     kindOther,
	}
	for _, n := range bankFiles {
		cases[n] = kindBank
	}

	for name, want := range cases {
		if got := classifyMemoryFile(dir, name); got != want {
			t.Errorf("classifyMemoryFile(%q) = %v, want %v", name, got, want)
		}
	}
}
