package pi

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Record extras keys: the wanted pinned packages and the specs the owner approved to ship code.
const (
	extraPackages         = "packages"
	extraPackagesApproved = "packages_approved"
)

// PackageKind is the source type of a Pi package spec.
type PackageKind string

// The spec kinds Pi's `pi install` takes that the harness pins.
const (
	KindNPM   PackageKind = "npm"
	KindGit   PackageKind = "git"
	KindLocal PackageKind = "local"
)

// Package is one parsed pin; Spec is passed to `pi install` unchanged.
type Package struct {
	Spec    string
	Kind    PackageKind
	Name    string // npm name, git host/owner/repo, or the cleaned local path
	Version string // exact semver or full commit; empty for local
}

var (
	npmSpec   = regexp.MustCompile(`^npm:((?:@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*)@(\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?(?:\+[0-9A-Za-z.-]+)?)$`)
	gitSpec   = regexp.MustCompile(`^git:([A-Za-z0-9][A-Za-z0-9.-]*/[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*)@([0-9a-fA-F]{40})$`)
	localChar = regexp.MustCompile(`^[A-Za-z0-9._/\\:-]+$`)
)

// ParsePackages parses a comma-separated pin list; "" is an empty list. Parsing is pure: no file or network access.
func ParsePackages(s string) ([]Package, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []Package
	seen := map[string]string{}
	for item := range strings.SplitSeq(s, ",") {
		p, err := parsePackage(item)
		if err != nil {
			return nil, err
		}
		key := string(p.Kind) + ":" + p.Name
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("package %q: duplicate of %q (one pin per package)", item, prev)
		}
		seen[key] = item
		out = append(out, p)
	}
	return out, nil
}

func parsePackage(item string) (Package, error) {
	switch {
	case item == "":
		return Package{}, errors.New("package list has an empty item")
	case strings.ContainsFunc(item, func(r rune) bool { return r <= ' ' || r == 0x7f }):
		return Package{}, fmt.Errorf("package %q: whitespace and control characters are not allowed", item)
	case strings.HasPrefix(item, "npm:"):
		return parseNPM(item)
	case strings.HasPrefix(item, "git:"):
		return parseGit(item)
	case strings.HasPrefix(item, "./") || filepath.IsAbs(item) || strings.HasPrefix(item, "/"):
		return parseLocal(item)
	}
	return Package{}, fmt.Errorf("package %q: want npm:<name>@<x.y.z>, git:<host>/<owner>/<repo>@<40-hex commit> or an absolute or ./relative path", item)
}

func parseNPM(item string) (Package, error) {
	m := npmSpec.FindStringSubmatch(item)
	if m == nil {
		return Package{}, fmt.Errorf("package %q: npm needs npm:<name>@<exact x.y.z>; a missing version, range, wildcard or "+
			"tag (^ ~ > < * x latest next) floats and is refused", item)
	}
	return Package{Spec: item, Kind: KindNPM, Name: m[1], Version: m[2]}, nil
}

func parseGit(item string) (Package, error) {
	m := gitSpec.FindStringSubmatch(item)
	if m == nil {
		return Package{}, fmt.Errorf("package %q: git needs git:<host>/<owner>/<repo>@<full 40-hex commit>; branches and tags "+
			"are movable, a commit hash is the only immutable git ref", item)
	}
	return Package{Spec: item, Kind: KindGit, Name: m[1], Version: strings.ToLower(m[2])}, nil
}

func parseLocal(item string) (Package, error) {
	if !localChar.MatchString(item) {
		return Package{}, fmt.Errorf("package %q: a local path allows only letters, digits and the characters : . _ - / and backslash", item)
	}
	if slicesHasDotDot(item) {
		return Package{}, fmt.Errorf("package %q: a local path must not contain '..'", item)
	}
	return Package{Spec: item, Kind: KindLocal, Name: filepath.Clean(item)}, nil
}

func slicesHasDotDot(p string) bool {
	return slices.Contains(strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }), "..")
}

// LocalPath is a local package's absolute path: a relative one resolves against configDir, never the cwd,
// so the same record installs the same files from any workspace.
func (p Package) LocalPath(configDir string) string {
	if filepath.IsAbs(p.Name) {
		return p.Name
	}
	return filepath.Join(configDir, p.Name)
}

// Approved reports whether spec is in approved by exact string: approving 1.2.3 does not approve 1.2.4.
func Approved(spec string, approved []Package) bool {
	for _, a := range approved {
		if a.Spec == spec {
			return true
		}
	}
	return false
}

func validatePackages(val string) error {
	_, err := ParsePackages(val)
	return err
}
