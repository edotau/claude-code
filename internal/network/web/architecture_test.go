package web

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeArchFile(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type archResp struct {
	Curated json.RawMessage `json:"curated"`
	Scan    archScan        `json:"scan"`
	Verbs   []any           `json:"verbs"`
}

func getArch(t *testing.T, s *Server) (archResp, string) {
	t.Helper()
	w := do(s, newReq(t, http.MethodGet, "/api/architecture", "127.0.0.1:7777"))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/architecture = %d, want 200: %s", w.Code, w.Body)
	}
	var r archResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	return r, w.Body.String()
}

func archDirByPath(sc archScan, p string) (archDir, bool) {
	for _, d := range sc.Dirs {
		if d.Path == p {
			return d, true
		}
	}
	return archDir{}, false
}

// writeArchFixture lays out a module with a leaf, an a<->b import cycle, a parse error, an empty dir and testdata.
func writeArchFixture(t *testing.T, root string) {
	t.Helper()
	writeArchFile(t, root, "go.mod", "module example.com/fx\n\ngo 1.22\n")
	writeArchFile(t, root, "internal/c/c.go", "// Package c is a leaf.\npackage c\n")
	writeArchFile(t, root, "internal/a/a.go", "package a\n\nimport _ \"example.com/fx/internal/c\"\nimport _ \"example.com/fx/internal/b\"\n")
	writeArchFile(t, root, "internal/a/a_test.go", "package a\n\nimport _ \"example.com/fx/internal/c\"\n")
	writeArchFile(t, root, "internal/a/testdata/x.go", "package x\n")
	writeArchFile(t, root, "internal/b/b.go", "package b\n\nimport _ \"example.com/fx/internal/a\"\nimport _ \"github.com/x/y\"\n")
	writeArchFile(t, root, "internal/bad/bad.go", "package bad\n\nimport (\n")
	if err := os.MkdirAll(filepath.Join(root, "internal", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeArchFile(t, root, "skills/synced/child/f.txt", "x")
	writeArchFile(t, root, "skills/other/child/f.txt", "x")
}

// checkArchDirs asserts the listed directories: the Go ones present, no testdata, relative paths, no empty dir.
func checkArchDirs(t *testing.T, sc archScan) {
	t.Helper()
	for _, p := range []string{"internal/bad", "internal/c"} {
		if _, ok := archDirByPath(sc, p); !ok {
			t.Errorf("dirs missing %s", p)
		}
	}
	for _, d := range sc.Dirs {
		if strings.Contains(d.Path, "testdata") || filepath.IsAbs(d.Path) {
			t.Errorf("unexpected dir path %q", d.Path)
		}
	}
	if _, ok := archDirByPath(sc, "internal/empty"); ok {
		t.Errorf("dirs must hold only directories with Go files")
	}
}

// checkArchScanShape asserts the scan-level facts: parse errors, listed dirs and the files list.
func checkArchScanShape(t *testing.T, r archResp) {
	t.Helper()
	if r.Scan.ParseErrors != 1 {
		t.Errorf("parseErrors = %d, want 1", r.Scan.ParseErrors)
	}
	checkArchDirs(t, r.Scan)
	if len(r.Scan.Files) != 3 || r.Scan.Files[0] != (archFile{Path: "README.md"}) || r.Scan.Files[1] != (archFile{Path: "internal/a/a.go", Lines: 4}) {
		t.Errorf("files = %+v, want sorted list with lines on .go only", r.Scan.Files)
	}
}

// checkArchLayers asserts layer, doc, import and external facts of the c, a and b fixture packages.
func checkArchLayers(t *testing.T, r archResp) {
	t.Helper()
	c, _ := archDirByPath(r.Scan, "internal/c")
	a, _ := archDirByPath(r.Scan, "internal/a")
	b, _ := archDirByPath(r.Scan, "internal/b")
	if c.Layer != 0 || !c.HasDoc || strings.Join(c.ImportedBy, ",") != "internal/a" {
		t.Errorf("leaf c = %+v, want layer 0, doc, importedBy [internal/a]", c)
	}
	if a.Layer < 1 || len(a.TestImports) != 1 || a.TestFiles != 1 || a.Files != 1 {
		t.Errorf("a = %+v, want layer>=1, one test import, 1 file + 1 test file", a)
	}
	checkArchImports(t, a, b)
}

func checkArchImports(t *testing.T, a, b archDir) {
	t.Helper()
	if strings.Join(a.ImportedBy, ",") != "internal/b" || len(b.External) != 1 || b.External[0] != "github.com/x/y" {
		t.Errorf("a.importedBy=%v b.external=%v", a.ImportedBy, b.External)
	}
}

func TestArchitectureEndToEnd(t *testing.T) {
	root := t.TempDir()
	writeArchFixture(t, root)
	orig := gitListFiles
	gitListFiles = func(string) ([]string, error) {
		return []string{"internal/a/a.go", "internal/c/c.go", "README.md"}, nil
	}
	t.Cleanup(func() { gitListFiles = orig })
	s := newTestServer(t, Options{ClaudeDir: root, Verbs: func() any { return []any{"v"} }}, nil)
	r, body := getArch(t, s) // a<->b cycle must not hang the layer pass

	if strings.Contains(body, root) {
		t.Errorf("body leaks absolute tempdir %q", root)
	}
	if len(r.Verbs) != 1 {
		t.Errorf("verbs = %v, want the injected tree", r.Verbs)
	}
	checkArchScanShape(t, r)
	checkArchLayers(t, r)
}

func TestArchitectureReadDirFailureIsNotCached(t *testing.T) {
	root := t.TempDir()
	writeArchFile(t, root, "internal/blocked/leaf.go", "package blocked\n")
	blocked := filepath.Join(root, "internal", "blocked")
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(blocked, 0o755); err != nil {
			t.Error(err)
		}
	})
	if _, err := os.ReadDir(blocked); err == nil {
		t.Skip("directory permissions are not enforced for this user")
	}
	var scan archScan
	if err := scanArchitecture(root, &scan); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("scanArchitecture error = %v, want permission denied", err)
	}
	s := newTestServer(t, Options{ClaudeDir: root}, nil)
	w := do(s, newReq(t, http.MethodGet, "/api/architecture", "127.0.0.1:7777"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("unreadable subtree status = %d, want 500", w.Code)
	}
	if s.arch.body != nil {
		t.Fatal("partial architecture scan was cached as successful")
	}
	if err := os.Chmod(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	r, _ := getArch(t, s)
	if _, ok := archDirByPath(r.Scan, "internal/blocked"); !ok {
		t.Fatal("retry after restoring access did not include the recovered subtree")
	}
}

func TestArchitectureModuleDirectivePreservesImportEdges(t *testing.T) {
	for name, directive := range map[string]string{
		"tab":     "module\texample.com/fx\n",
		"comment": "module example.com/fx // c\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeArchFile(t, root, "go.mod", directive)
			writeArchFile(t, root, "internal/a/a.go", "package a\nimport _ \"example.com/fx/internal/b\"\n")
			writeArchFile(t, root, "internal/b/b.go", "package b\n")
			var scan archScan
			if err := scanArchitecture(root, &scan); err != nil {
				t.Fatal(err)
			}
			a, _ := archDirByPath(scan, "internal/a")
			b, _ := archDirByPath(scan, "internal/b")
			if got := strings.Join(a.Imports, ","); got != "internal/b" {
				t.Errorf("a imports = %q, want internal/b", got)
			}
			if got := strings.Join(b.ImportedBy, ","); got != "internal/a" {
				t.Errorf("b importedBy = %q, want internal/a", got)
			}
		})
	}
}

func TestArchitectureMissingInternal(t *testing.T) {
	s := newTestServer(t, Options{ClaudeDir: t.TempDir()}, nil)
	r, body := getArch(t, s)
	if r.Scan.Error != "source tree not found" {
		t.Errorf("scan.error = %q", r.Scan.Error)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["verbs"]; ok {
		t.Errorf("verbs must be omitted when nil, got keys %v", top)
	}
	if strings.Contains(body, s.opts.ClaudeDir) {
		t.Errorf("error body leaks the tempdir path")
	}
}

func TestArchitectureRoutesGuarded(t *testing.T) {
	s := newTestServer(t, Options{ClaudeDir: t.TempDir()}, nil)
	for _, p := range []string{"/api/architecture", "/assets/architecture.js"} {
		if w := do(s, newReq(t, http.MethodGet, p, "evil.example.com")); w.Code != http.StatusMisdirectedRequest {
			t.Errorf("%s with bad Host = %d, want 421", p, w.Code)
		}
	}
	w := do(s, newReq(t, http.MethodGet, "/assets/architecture.js", "127.0.0.1:7777"))
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "javascript") || w.Body.Len() == 0 {
		t.Errorf("architecture.js = %d %q len %d", w.Code, w.Header().Get("Content-Type"), w.Body.Len())
	}
}

type curatedDoc struct {
	Dirs map[string]struct {
		Purpose  string
		Proposal []string
	} `json:"dirs"`
	Findings []struct{ ID string } `json:"findings"`
	Proposal []struct{ ID string } `json:"proposals"`
}

// goPackageDirs lists the slash-relative directories under root's parent that hold Go files, skipping vendored names.
func goPackageDirs(t *testing.T, root string) []string {
	t.Helper()
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if p != root && skipArchName(d.Name()) {
			return fs.SkipDir
		}
		ents, _ := os.ReadDir(p)
		hasGo := false
		for _, e := range ents {
			hasGo = hasGo || (!e.IsDir() && strings.HasSuffix(e.Name(), ".go"))
		}
		if hasGo {
			rel, _ := filepath.Rel(filepath.Join(root, ".."), p)
			dirs = append(dirs, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

func loadCuratedDoc(t *testing.T) curatedDoc {
	t.Helper()
	raw, err := assetsFS.ReadFile("assets/architecture.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc curatedDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestArchitectureCuratedCoversRealPackages(t *testing.T) {
	doc := loadCuratedDoc(t)
	for _, rel := range goPackageDirs(t, filepath.Join("..", "..", "..", "internal")) {
		if e, ok := doc.Dirs[rel]; !ok || strings.TrimSpace(e.Purpose) == "" {
			t.Errorf("architecture.json dirs[%q] missing or empty purpose", rel)
		}
	}
}

func TestArchitectureCuratedIDsAreUniqueAndReferenced(t *testing.T) {
	doc := loadCuratedDoc(t)
	props := map[string]bool{}
	for _, p := range doc.Proposal {
		if props[p.ID] {
			t.Errorf("duplicate proposal id %s", p.ID)
		}
		props[p.ID] = true
	}
	seen := map[string]bool{}
	for _, f := range doc.Findings {
		if seen[f.ID] {
			t.Errorf("duplicate finding id %s", f.ID)
		}
		seen[f.ID] = true
	}
	for dir, e := range doc.Dirs {
		for _, p := range e.Proposal {
			if !props[p] {
				t.Errorf("dirs[%q] references unknown proposal %s", dir, p)
			}
		}
	}
}
