package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRtkMissWordDropsAttachedValues(t *testing.T) {
	for in, want := range map[string]string{"grep -ePAT x": "grep", "curl -uUSER:pw": "curl", "curl -sL u": "curl", "grep -n a": "grep -n", "x --k=v": "x", "ls --all": "ls --all"} {
		if got := rtkMissWord(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestReadDreamRtkFakeBinary(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\necho '{\"supported\":[{\"command\":\"grep -n /secret/path\",\"count\":7},{\"command\":\"head -c 5 f\",\"count\":2}]}'\n"
	if err := os.WriteFile(filepath.Join(dir, "rtk"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := readDreamRtk()
	if err != nil || len(got) != 2 || got[0].Command != "grep -n" || got[1].Command != "head -c" || got[0].Count != 7 {
		t.Fatalf("got %v, %v", got, err)
	}
}
