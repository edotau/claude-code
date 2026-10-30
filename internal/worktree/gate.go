package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// registryPkgs iterate the hook, setting, verb and schema registries, so any change can break them.
var registryPkgs = []string{"internal/hooks", "internal/hookspec", "internal/settings", "internal/cli", "internal/schemas"}

// goPkg is the slice of `go list -json` the gate reads.
type goPkg struct {
	ImportPath, Dir                    string
	Imports, TestImports, XTestImports []string
}

// ScopedGoGate builds the merge gate: build and vet everything, test the packages base...HEAD changed, every
// package that imports them (tests included, transitively) and the registry packages. full is true when a
// go.mod/go.sum change makes that unsafe.
func ScopedGoGate(ctx context.Context, dir, base string) (cmd string, pkgs []string, full bool, err error) {
	out, err := runGit(ctx, dir, nil, "diff", "--name-only", "-z", base+"...HEAD")
	if err != nil {
		return "", nil, false, fmt.Errorf("git diff %s...HEAD: %w (%s)", base, err, gitErrorDetail(err))
	}
	changed := map[string]bool{}
	for _, f := range strings.Split(string(out), "\x00") {
		switch {
		case f == "":
		case f == "go.mod" || f == "go.sum":
			return "go build ./... && go vet ./... && go test ./...", nil, true, nil
		default:
			if p := goPkgOf(dir, filepath.Dir(f)); p != "" {
				changed[p] = true
			}
		}
	}
	list, err := listPackages(ctx, dir)
	if err != nil {
		return "", nil, false, err
	}
	abs, err := filepath.Abs(dir) // go list reports absolute package dirs
	if err != nil {
		return "", nil, false, err
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
