package worktree

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// registryPkgs iterate the hook, setting, verb and schema registries, so any change can break them.
var registryPkgs = []string{"internal/hooks", "internal/hookspec", "internal/settings", "internal/cli", "internal/schemas"}

// ScopedGoGate builds the merge gate: build and vet everything, test only the packages base...HEAD changed
// plus the registry packages. full is true when a go.mod/go.sum change makes that unsafe.
func ScopedGoGate(ctx context.Context, dir, base string) (cmd string, pkgs []string, full bool, err error) {
	out, err := runGit(ctx, dir, nil, "diff", "--name-only", "-z", base+"...HEAD")
	if err != nil {
		return "", nil, false, fmt.Errorf("git diff %s...HEAD: %w (%s)", base, err, gitErrorDetail(err))
	}
	set := map[string]bool{}
	for _, p := range registryPkgs {
		set[p] = goPkgOf(dir, p) == p // a repo without the harness registries skips them
	}
	for _, f := range strings.Split(string(out), "\x00") {
		switch {
		case f == "":
		case f == "go.mod" || f == "go.sum":
			return "go build ./... && go vet ./... && go test ./...", nil, true, nil
		default:
			if p := goPkgOf(dir, filepath.Dir(f)); p != "" {
				set[p] = true
			}
		}
	}
	for p, ok := range set {
		if ok {
			pkgs = append(pkgs, p)
		}
	}
	slices.Sort(pkgs)
	args := make([]string, len(pkgs))
	for i, p := range pkgs {
		args[i] = "./" + p
	}
	return "go build ./... && go vet ./... && go test " + strings.Join(args, " "), pkgs, false, nil
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
