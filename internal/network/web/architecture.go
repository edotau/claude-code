package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const archTTL = 60 * time.Second

var archGoRoots = []string{"internal", "cmd", "defaults"}

const gitListTimeout = 10 * time.Second

// gitListFiles lists the tracked and unignored files under root, slash-separated and relative; tests swap it.
var gitListFiles = func(root string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitListTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for f := range bytes.SplitSeq(out, []byte{0}) {
		if len(f) > 0 {
			files = append(files, string(f))
		}
	}
	return files, nil
}

// archCache holds the last /api/architecture body; the lock spans the build so concurrent callers coalesce.
type archCache struct {
	mu   sync.Mutex
	body []byte
	at   time.Time
}

func (c *archCache) get(now time.Time, build func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.body != nil && now.Sub(c.at) < archTTL {
		return c.body, nil
	}
	body, err := build()
	if err != nil {
		return nil, err
	}
	c.body, c.at = body, now
	return body, nil
}

// archDir is one scanned directory; every slice is non-nil so the JSON carries [] rather than null.
type archDir struct {
	Path        string   `json:"path"`
	Package     string   `json:"package"`
	Files       int      `json:"files"`
	TestFiles   int      `json:"testFiles"`
	LOC         int      `json:"loc"`
	Imports     []string `json:"imports"`
	TestImports []string `json:"testImports"`
	ImportedBy  []string `json:"importedBy"`
	External    []string `json:"external"`
	Layer       int      `json:"layer"`
	HasDoc      bool     `json:"hasDoc"`
}

// archFile is one tracked or unignored file; Lines is set for .go files only.
type archFile struct {
	Path  string `json:"path"`
	Lines int    `json:"lines,omitempty"`
}

type archScan struct {
	Dirs        []archDir  `json:"dirs"`
	Files       []archFile `json:"files"`
	ParseErrors int        `json:"parseErrors"`
	Error       string     `json:"error,omitempty"`
}

type archBody struct {
	Curated json.RawMessage `json:"curated"`
	Scan    archScan        `json:"scan"`
	Verbs   any             `json:"verbs,omitempty"`
}

func (s *Server) handleArchitecture(w http.ResponseWriter, _ *http.Request) {
	body, err := s.arch.get(s.now(), s.buildArchitecture)
	if err != nil {
		s.logger.Printf("architecture: %v", err)
		http.Error(w, "architecture unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

func (s *Server) handleArchitectureJS(w http.ResponseWriter, r *http.Request) {
	body, err := assetsFS.ReadFile("assets/architecture.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(body)
}

func (s *Server) buildArchitecture() ([]byte, error) {
	curated, err := assetsFS.ReadFile("assets/architecture.json")
	if err != nil || !json.Valid(curated) {
		s.logger.Printf("architecture: curated json unavailable: %v", err)
		curated = []byte("{}")
	}
	out := archBody{Curated: curated, Scan: archScan{Dirs: []archDir{}, Files: []archFile{}}}
	if s.opts.Verbs != nil {
		out.Verbs = s.opts.Verbs()
	}
	if err := scanArchitecture(s.opts.ClaudeDir, &out.Scan); err != nil {
		s.logger.Printf("architecture: scan: %v", err)
		out.Scan.Error = "source tree not found"
	}
	return json.Marshal(out)
}

// scanArchitecture fills sc from the repo at root; an error means there is no internal/ tree to scan.
func scanArchitecture(root string, sc *archScan) error {
	if st, err := os.Stat(filepath.Join(root, "internal")); err != nil || !st.IsDir() {
		if err == nil {
			err = fs.ErrNotExist
		}
		return err
	}
	mod := modulePath(root)
	for _, r := range archGoRoots {
		walkGoDirs(root, r, mod, sc)
	}
	linkArchitecture(sc.Dirs)
	listArchFiles(root, sc)
	return nil
}

// listArchFiles fills sc.Files from git so .gitignore decides what renders; a git failure sets sc.Error.
func listArchFiles(root string, sc *archScan) {
	list, err := gitListFiles(root)
	if err != nil {
		sc.Error = "file list unavailable (git ls-files failed)"
		return
	}
	sort.Strings(list)
	for _, f := range list {
		af := archFile{Path: f}
		if strings.HasSuffix(f, ".go") {
			af.Lines = countLines(filepath.Join(root, filepath.FromSlash(f)))
		}
		sc.Files = append(sc.Files, af)
	}
}

func modulePath(root string) string {
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}

func skipArchName(n string) bool {
	return n == "testdata" || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_")
}

// walkGoDirs emits each directory under rel (and rel itself) that holds Go files.
func walkGoDirs(root, rel, mod string, sc *archScan) {
	entries, err := os.ReadDir(filepath.Join(root, rel))
	if err != nil {
		return
	}
	if hasGoFile(entries) {
		sc.Dirs = append(sc.Dirs, scanGoDir(root, rel, mod, entries, sc))
	}
	for _, e := range entries {
		if e.IsDir() && !skipArchName(e.Name()) {
			walkGoDirs(root, path.Join(rel, e.Name()), mod, sc)
		}
	}
}

func hasGoFile(entries []os.DirEntry) bool {
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}

func scanGoDir(root, rel, mod string, entries []os.DirEntry, sc *archScan) archDir {
	d := archDir{Path: rel, Imports: []string{}, TestImports: []string{}, ImportedBy: []string{}, External: []string{}}
	imps, tests, ext := map[string]bool{}, map[string]bool{}, map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(rel), e.Name())
		f, err := parser.ParseFile(fset, full, nil, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			sc.ParseErrors++
			continue
		}
		isTest := strings.HasSuffix(e.Name(), "_test.go")
		if isTest {
			d.TestFiles++
		} else {
			d.Files++
			d.LOC += countLines(full)
			d.HasDoc = d.HasDoc || hasPackageDoc(f)
		}
		if d.Package == "" || (!isTest && strings.HasSuffix(d.Package, "_test")) {
			d.Package = f.Name.Name
		}
		collectImports(f, mod, isTest, imps, tests, ext)
	}
	d.Imports, d.TestImports, d.External = sortedKeys(imps), sortedKeys(tests), sortedKeys(ext)
	return d
}

func hasPackageDoc(f *ast.File) bool {
	return f.Doc != nil && strings.HasPrefix(f.Doc.Text(), "Package "+f.Name.Name)
}

func collectImports(f *ast.File, mod string, isTest bool, imps, tests, ext map[string]bool) {
	for _, im := range f.Imports {
		p := strings.Trim(im.Path.Value, "\"`")
		if rel, ok := strings.CutPrefix(p, mod+"/"); ok && mod != "" {
			if isTest {
				tests[rel] = true
			} else {
				imps[rel] = true
			}
			continue
		}
		first, _, _ := strings.Cut(p, "/")
		if !isTest && p != mod && strings.Contains(first, ".") {
			ext[p] = true
		}
	}
}

func countLines(file string) int {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0
	}
	n := strings.Count(string(b), "\n")
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	return n
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// linkArchitecture fills ImportedBy and Layer.
func linkArchitecture(dirs []archDir) {
	idx := make(map[string]int, len(dirs))
	for i, d := range dirs {
		idx[d.Path] = i
	}
	for _, d := range dirs {
		for _, imp := range d.Imports {
			if j, ok := idx[imp]; ok {
				dirs[j].ImportedBy = append(dirs[j].ImportedBy, d.Path)
			}
		}
	}
	for i := range dirs {
		sort.Strings(dirs[i].ImportedBy)
	}
	fillLayers(dirs, idx)
}

// fillLayers sets Layer: the longest import path to a leaf; a cycle edge counts as a leaf.
func fillLayers(dirs []archDir, idx map[string]int) {
	memo, visiting := map[string]int{}, map[string]bool{}
	var layer func(p string) int
	layer = func(p string) int {
		if v, ok := memo[p]; ok {
			return v
		}
		j, ok := idx[p]
		if !ok || visiting[p] {
			return -1
		}
		visiting[p] = true
		best := 0
		for _, imp := range dirs[j].Imports {
			if l := layer(imp); l >= 0 && l+1 > best {
				best = l + 1
			}
		}
		visiting[p] = false
		memo[p] = best
		return best
	}
	for i := range dirs {
		dirs[i].Layer = layer(dirs[i].Path)
	}
}
