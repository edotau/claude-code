// Advisory rows for a change: new tests, new helpers with their caller counts, and flag growth per verb.

package source

import (
	"bufio"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GoChange is one changed Go file under the repo root; From is its rename or copy source.
type GoChange struct{ Path, From string }

// Advisory holds the printable rows: New for new tests and helpers, Flags for per-verb flag growth.
type Advisory struct{ New, Flags []string }

type goPair struct {
	path       string
	base, head *ast.File
}

// advisor holds what one GoAdvisory run shares: the parse set, the repo and the per-package file cache.
type advisor struct {
	fset  *token.FileSet
	root  string
	pairs []goPair
	dirs  map[string][]*ast.File
}

// GoAdvisory compares each change at baseRev with the index (staged) or the worktree; errors yield no rows.
func GoAdvisory(root string, env []string, baseRev string, staged bool, changes []GoChange) Advisory {
	var kept []GoChange
	for _, c := range changes {
		if strings.HasSuffix(c.Path, ".go") {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		return Advisory{}
	}
	a := &advisor{fset: token.NewFileSet(), root: root, dirs: map[string][]*ast.File{}}
	a.readPairs(env, baseRev, staged, kept)
	var adv Advisory
	for _, p := range a.pairs {
		adv.New = append(adv.New, a.newDeclRows(p)...)
	}
	adv.Flags = flagGrowthRows(a.pairs)
	return adv
}

func (a *advisor) readPairs(env []string, baseRev string, staged bool, cs []GoChange) {
	var queries []string
	for _, c := range cs {
		queries = append(queries, baseRev+":./"+cmpPath(c.From, c.Path))
		if staged {
			queries = append(queries, ":./"+c.Path)
		}
	}
	blobs := catBatch(a.root, env, queries)
	for i, c := range cs {
		p := goPair{path: c.Path}
		if staged {
			p.base, p.head = parseGo(a.fset, c.Path, blobs[2*i]), parseGo(a.fset, c.Path, blobs[2*i+1])
		} else {
			data, _ := os.ReadFile(filepath.Join(a.root, filepath.FromSlash(c.Path)))
			p.base, p.head = parseGo(a.fset, c.Path, blobs[i]), parseGo(a.fset, c.Path, data)
		}
		if p.head != nil {
			a.pairs = append(a.pairs, p)
		}
	}
}

func cmpPath(from, path string) string {
	if from != "" {
		return from
	}
	return path
}

func parseGo(fset *token.FileSet, name string, src []byte) *ast.File {
	if src == nil {
		return nil
	}
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	return f
}

// catBatch reads every object in one `git cat-file --batch`; a missing or unreadable object is a nil entry.
func catBatch(root string, env []string, queries []string) [][]byte {
	out := make([][]byte, len(queries))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "cat-file", "--batch")
	cmd.Env = env
	cmd.Stdin = strings.NewReader(strings.Join(queries, "\n") + "\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return out
	}
	r := bufio.NewReader(stdout)
	for i := range queries {
		hdr, err := r.ReadString('\n')
		if err != nil {
			break
		}
		f := strings.Fields(hdr)
		if len(f) != 3 {
			continue
		}
		n, err := strconv.Atoi(f[2])
		if err != nil {
			break
		}
		buf := make([]byte, n+1)
		if _, err := io.ReadFull(r, buf); err != nil {
			break
		}
		out[i] = buf[:n]
	}
	_, _ = io.Copy(io.Discard, stdout)
	_ = cmd.Wait()
	return out
}

func freeFuncs(f *ast.File) map[string]*ast.FuncDecl {
	m := map[string]*ast.FuncDecl{}
	if f == nil {
		return m
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
			m[fd.Name.Name] = fd
		}
	}
	return m
}

func isTestName(name string) bool {
	for _, pre := range []string{"Test", "Benchmark", "Fuzz"} {
		if rest, ok := strings.CutPrefix(name, pre); ok {
			return rest == "" || !strings.ContainsFunc(rest[:1], func(r rune) bool { return r >= 'a' && r <= 'z' })
		}
	}
	return false
}

// newDecl is the head function when it is new, a test or helper candidate (not a method, init or main).
func newDecl(d ast.Decl, old map[string]*ast.FuncDecl) *ast.FuncDecl {
	fd, ok := d.(*ast.FuncDecl)
	if !ok || fd.Recv != nil || old[fd.Name.Name] != nil {
		return nil
	}
	if n := fd.Name.Name; n == "init" || n == "main" || n == "_" {
		return nil
	}
	return fd
}

func (a *advisor) newDeclRows(p goPair) []string {
	old := freeFuncs(p.base)
	var rows []string
	for _, d := range p.head.Decls {
		fd := newDecl(d, old)
		if fd == nil {
			continue
		}
		name := fd.Name.Name
		at := fmt.Sprintf("%s:%d:", p.path, a.fset.Position(fd.Pos()).Line)
		switch {
		case strings.HasSuffix(p.path, "_test.go") && isTestName(name):
			rows = append(rows, at+" new test "+name)
		case !ast.IsExported(name):
			rows = append(rows, fmt.Sprintf("%s new helper %s (%d callers)", at, name, countUses(a.packageFiles(p), name)))
		}
	}
	return rows
}

// packageFiles is the same-package files of p's directory: changed ones at head, the rest from disk.
func (a *advisor) packageFiles(p goPair) []*ast.File {
	dir := filepath.ToSlash(filepath.Dir(p.path))
	key := dir + "\x00" + p.head.Name.Name
	if fs, ok := a.dirs[key]; ok {
		return fs
	}
	changed := map[string]*ast.File{}
	for _, q := range a.pairs {
		changed[q.path] = q.head
	}
	var files []*ast.File
	entries, _ := os.ReadDir(filepath.Join(a.root, filepath.FromSlash(dir)))
	for _, e := range entries {
		rel := filepath.ToSlash(filepath.Join(dir, e.Name()))
		f := changed[rel]
		if f == nil && strings.HasSuffix(e.Name(), ".go") && !e.IsDir() {
			data, err := os.ReadFile(filepath.Join(a.root, filepath.FromSlash(rel)))
			if err == nil {
				f = parseGo(a.fset, rel, data)
			}
		}
		if f != nil && f.Name.Name == p.head.Name.Name {
			files = append(files, f)
		}
	}
	a.dirs[key] = files
	return files
}

// countUses counts identifier uses of the free function name, skipping its declaration, selectors and methods.
func countUses(files []*ast.File, name string) int {
	n := 0
	skip := map[*ast.Ident]bool{}
	for _, f := range files {
		ast.Inspect(f, func(node ast.Node) bool {
			switch x := node.(type) {
			case *ast.SelectorExpr:
				skip[x.Sel] = true
			case *ast.FuncDecl:
				skip[x.Name] = true
				return x.Recv != nil || x.Name.Name != name
			case *ast.Ident:
				if x.Name == name && !skip[x] {
					n++
				}
			}
			return true
		})
	}
	return n
}
