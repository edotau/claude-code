package ask

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Given pack flags that cannot work together or alone, askPack refuses each; valid ones build the spec the request carries.
func TestAskPackFlagCombos(t *testing.T) {
	refused := []struct {
		name string
		args []string
		want string
	}{
		{"index alone", []string{"--index"}, "--index needs"},
		{"max-files alone", []string{"--max-files", "3"}, "--max-files needs"},
		{"max-file-bytes alone", []string{"--max-file-bytes", "9"}, "--max-file-bytes needs"},
		{"negative max-files", []string{"--dirs", ".", "--max-files", "-1"}, "negative"},
		{"batch", []string{"--dirs", ".", "--batch", "-"}, "--batch"},
		{"live", []string{"--files", "*.go", "--live"}, "--live"},
		{"host", []string{"--diff", "--host", "box"}, "--host"},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			fs, f := askFlagSet()
			if err := fs.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			if _, err := askPack(fs, f); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("askPack(%v) = %v, want an error containing %q", tt.args, err, tt.want)
			}
		})
	}
	fs, f := askFlagSet()
	if err := fs.Parse([]string{"--dirs", "a,b", "--index", "--max-files", "0", "--diff"}); err != nil {
		t.Fatal(err)
	}
	f.pack, _ = askPack(fs, f)
	req := (&askRun{f: f}).request("task")
	if p := req.Pack; p == nil || len(p.Dirs) != 2 || !p.Diff || !p.Index || !p.IndexOnly {
		t.Fatalf("request.Pack = %+v, want dirs a,b + diff + index + index-only", p)
	}
}

// Ties the --effort help to the harness scale.
func TestEffortFlagHelpListsTheScale(t *testing.T) {
	fs, _ := askFlagSet()
	if got := fs.Lookup("effort").Usage; got != "reasoning effort: low|medium|high|xhigh|max" {
		t.Errorf("--effort usage = %q", got)
	}
}

// attachFlag.Set resolves a real file to an absolute path and errors on anything else: missing,
// directory, or a path flag.Abs itself cannot resolve.
func TestAttachFlagSet(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	writeFixtureFile(t, f, "hi")

	var a attachFlag
	if err := a.Set(f); err != nil {
		t.Fatalf("Set(%q): %v", f, err)
	}
	if len(a) != 1 || a[0] != f {
		t.Errorf("a = %q, want [%q]", a, f)
	}

	if err := a.Set(filepath.Join(dir, "missing.txt")); err == nil {
		t.Error("Set on a missing path: want an error, got nil")
	}
	if err := a.Set(dir); err == nil {
		t.Error("Set on a directory: want an error, got nil")
	}
}

// --keep-env comma-splits each occurrence and accumulates across repeats.
func TestKeepEnvFlagSet(t *testing.T) {
	fs, f := askFlagSet()
	if err := fs.Parse([]string{"--keep-env", "DATABRICKS_HOST,DATABRICKS_TOKEN", "--keep-env", "HOME"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !slices.Equal(f.keepEnv, []string{"DATABRICKS_HOST", "DATABRICKS_TOKEN", "HOME"}) {
		t.Errorf("keepEnv = %q, want accumulated comma-split names", f.keepEnv)
	}
}

// attachFlag.Set rejects binary files up front (NUL, invalid UTF-8) and accepts multibyte UTF-8 text.
func TestAttachFlagSetRejectsBinary(t *testing.T) {
	dir := t.TempDir()
	var a attachFlag
	for name, data := range map[string]string{"nul.bin": "ab\x00cd", "bad.dat": "\xff\xfe\x80"} {
		f := filepath.Join(dir, name)
		writeFixtureFile(t, f, data)
		err := a.Set(f)
		if err == nil || !strings.Contains(err.Error(), "binary file") || !strings.Contains(err.Error(), name) {
			t.Errorf("Set(%s): err = %v, want a binary-file error naming the path", name, err)
		}
	}
	if len(a) != 0 {
		t.Errorf("a = %q, want nothing recorded for rejected files", a)
	}
	f := filepath.Join(dir, "utf8.txt")
	writeFixtureFile(t, f, "héllo 世界")
	if err := a.Set(f); err != nil || len(a) != 1 {
		t.Errorf("Set(multibyte text): err = %v, a = %q, want accepted", err, a)
	}
}
