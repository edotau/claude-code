package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// registryPkgs iterate the hook, setting, verb and schema registries, so any change can break them.
var registryPkgs = []string{"internal/hooks", "internal/settings", "internal/agent", "internal/cli", "internal/schemas"}

// goPkg is the slice of `go list -json` the gate reads.
type goPkg struct {
	ImportPath, Dir                             string
	Imports, TestImports, XTestImports          []string
	EmbedFiles, TestEmbedFiles, XTestEmbedFiles []string
	TestGoFiles, XTestGoFiles                   []string
}

// ScopedGoGate builds the merge gate: build and vet everything, test the packages base...HEAD changed, every
// package that imports them (tests included, transitively) and the registry packages. full is true for a
// go.mod/go.sum change or an unowned non-markdown file.
func ScopedGoGate(ctx context.Context, dir, base string) (cmd string, pkgs []string, full bool, err error) {
	out, err := runGit(ctx, dir, nil, "diff", "--name-only", "-z", base+"...HEAD")
	if err != nil {
		return "", nil, false, fmt.Errorf("git diff %s...HEAD: %w (%s)", base, err, gitErrorDetail(err))
	}
	list, err := listPackages(ctx, dir)
	if err != nil {
		return "", nil, false, err
	}
	abs, err := filepath.Abs(dir) // go list reports absolute package dirs
	if err != nil {
		return "", nil, false, err
	}
	changed, src := map[string]bool{}, cachedReader()
	for f := range strings.SplitSeq(string(out), "\x00") {
		switch f {
		case "":
		case "go.mod", "go.sum":
			return fullGoGate, nil, true, nil
		default:
			owners := filePackages(list, abs, f, src)
			if len(owners) == 0 && !strings.HasSuffix(f, ".md") && !strings.HasSuffix(f, ".go") { // .go: build covers deletions
				return fullGoGate, nil, true, nil // an unowned non-markdown file may feed any test
			}
			for _, p := range owners {
				changed[p] = true
			}
		}
	}
	set := reverseDeps(list, abs, changed)
	for _, p := range registryPkgs {
		if goPkgOf(dir, p) == p { // a repo without the harness registries skips them
			set[p] = true
		}
	}
	for p := range set {
		pkgs = append(pkgs, p)
	}
	slices.Sort(pkgs)
	args := make([]string, len(pkgs))
	for i, p := range pkgs {
		args[i] = "./" + p
	}
	return "go build ./... && go vet ./... && go test " + strings.Join(args, " "), pkgs, false, nil
}

const fullGoGate = "go build ./... && go vet ./... && go test ./..."

// filePackages maps a changed file to its package dir; a non-Go file also to packages embedding it or tests naming it.
func filePackages(list []goPkg, abs, f string, src func(string) string) []string {
	var out []string
	if p := goPkgOf(abs, filepath.Dir(f)); p != "" {
		out = append(out, p)
	}
	if strings.HasSuffix(f, ".go") {
		return out
	}
	for _, p := range list {
		if r, err := filepath.Rel(abs, p.Dir); err == nil && refersTo(p, filepath.ToSlash(r), f, src) {
			out = append(out, filepath.ToSlash(r))
		}
	}
	return out
}

// refersTo reports whether p (at repo-relative r) embeds f or has a test file mentioning f or its directory.
func refersTo(p goPkg, r, f string, src func(string) string) bool {
	for _, e := range slices.Concat(p.EmbedFiles, p.TestEmbedFiles, p.XTestEmbedFiles) {
		if path.Join(r, filepath.ToSlash(e)) == f {
			return true
		}
	}
	d := path.Dir(f)
	for _, t := range slices.Concat(p.TestGoFiles, p.XTestGoFiles) {
		if b := src(filepath.Join(p.Dir, t)); strings.Contains(b, f) || d != "." && strings.Contains(b, d) {
			return true
		}
	}
	return false
}

// cachedReader returns a file reader that reads each path once; an unreadable file reads as "".
func cachedReader() func(string) string {
	seen := map[string]string{}
	return func(p string) string {
		if b, ok := seen[p]; ok {
			return b
		}
		b, _ := os.ReadFile(p)
		seen[p] = string(b)
		return seen[p]
	}
}

// listPackages runs `go list -e -json ./...` in dir and decodes its package stream.
func listPackages(ctx context.Context, dir string) ([]goPkg, error) {
	c := exec.CommandContext(ctx, "go", "list", "-e", "-json", "./...")
	c.Dir = dir
	raw, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("go list ./...: %w (%s)", err, gitErrorDetail(err))
	}
	var pkgs []goPkg
	for dec := json.NewDecoder(strings.NewReader(string(raw))); ; {
		var p goPkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			return pkgs, nil
		} else if err != nil {
			return nil, fmt.Errorf("go list ./...: %w", err)
		}
		pkgs = append(pkgs, p)
	}
}

// reverseDeps returns changed plus every package importing one transitively, plus those whose tests import any.
func reverseDeps(list []goPkg, dir string, changed map[string]bool) map[string]bool {
	rel := map[string]string{} // import path -> repo-relative dir
	for _, p := range list {
		if r, err := filepath.Rel(dir, p.Dir); err == nil {
			rel[p.ImportPath] = filepath.ToSlash(r)
		}
	}
	hit := func(imps []string, in map[string]bool) bool {
		return slices.ContainsFunc(imps, func(i string) bool { return in[rel[i]] })
	}
	affected := map[string]bool{}
	for p := range changed {
		affected[p] = true
	}
	for grew := true; grew; { // non-test imports propagate; a test import only selects its own package
		grew = false
		for _, p := range list {
			if r := rel[p.ImportPath]; !affected[r] && hit(p.Imports, affected) {
				affected[r], grew = true, true
			}
		}
	}
	out := map[string]bool{}
	for _, p := range list {
		if r := rel[p.ImportPath]; affected[r] || hit(p.TestImports, affected) || hit(p.XTestImports, affected) {
			out[r] = true
		}
	}
	return out
}

// goPkgOf returns the nearest existing directory at or above rel under dir that holds .go files, or "".
func goPkgOf(dir, rel string) string {
	for rel != "." && rel != "/" && rel != "" {
		if m, _ := filepath.Glob(filepath.Join(dir, rel, "*.go")); len(m) > 0 {
			return filepath.ToSlash(rel)
		}
		rel = filepath.Dir(rel)
	}
	return ""
}

// ChooseVerifyGate picks the post-integration gate: noVerify skips it, an explicit verify wins, else it is detected
// from worktreePath like workflows/green-keeper-loop.js (Makefile test -> go.mod -> pyproject -> package.json -> Cargo.toml).
func ChooseVerifyGate(worktreePath, verify string, noVerify bool) (gate, source string, err error) {
	if noVerify {
		return "", "", nil
	}
	if verify != "" {
		return verify, "flag", nil
	}
	gate, source = detectVerifyGate(worktreePath)
	if gate == "" {
		return "", "", errors.New(`no verification gate found — pass --verify "<cmd>" or --no-verify`)
	}
	return gate, source, nil
}

// detectVerifyGate mirrors workflows/green-keeper-loop.js's detection order over dir's own files.
func detectVerifyGate(dir string) (gate, source string) {
	if hasMakeTarget(filepath.Join(dir, "Makefile"), "test") {
		return "make test", "Makefile"
	}
	if paths.FileExists(filepath.Join(dir, "go.mod")) {
		return "go build ./... && go vet ./... && go test ./...", "go.mod"
	}
	if paths.FileExists(filepath.Join(dir, "pyproject.toml")) || paths.FileExists(filepath.Join(dir, "setup.cfg")) {
		return "pytest -q", "pyproject"
	}
	if hasNPMTestScript(filepath.Join(dir, "package.json")) {
		return "npm test", "package.json"
	}
	if paths.FileExists(filepath.Join(dir, "Cargo.toml")) {
		return "cargo build && cargo test", "Cargo.toml"
	}
	return "", ""
}

// hasMakeTarget reports whether makefile declares a target named exactly target; a missing Makefile is "no target".
func hasMakeTarget(makefile, target string) bool {
	data, err := os.ReadFile(makefile)
	if err != nil {
		return false
	}
	prefix := target + ":"
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// hasNPMTestScript reports whether pkgJSON declares a "test" script.
func hasNPMTestScript(pkgJSON string) bool {
	data, err := os.ReadFile(pkgJSON)
	if err != nil {
		return false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return false
	}
	_, ok := pkg.Scripts["test"]
	return ok
}

// RunVerify runs cmdStr in workDir under `bash -c 'set -o pipefail; ...'` so a failing pipeline stage fails the gate;
// ctx cancellation kills it, and a nonzero exit is the one signal that stops promotion.
func RunVerify(ctx context.Context, workDir, cmdStr string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "bash", "-c", "set -o pipefail; "+cmdStr)
	cmd.Dir = workDir
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}
