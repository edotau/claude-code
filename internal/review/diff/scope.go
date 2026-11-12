package diff

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/review/source"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// scopeBytesPerLine is the size estimate per changed line for the --range listing.
const scopeBytesPerLine = 80

// scopeExts are the source extensions the --paths listing keeps.
var scopeExts = setOf(".go", ".py", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".rs", ".java")

// scopeSkipDirs are directories the --paths listing never lists, even when a path names one.
var scopeSkipDirs = setOf("node_modules", ".venv", "vendor", "testdata")

// scopeFile is one listed file with its size in bytes.
type scopeFile struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// scopePackage is one Go package dir among the listed files, with the in-repo dirs that import it.
type scopePackage struct {
	Dir       string   `json:"dir"`
	Files     []string `json:"files"`
	Importers []string `json:"importers"`
}

type scopeResult struct {
	Files    []scopeFile    `json:"files"`
	Packages []scopePackage `json:"packages"`
}

// RunScope lists the files a change touches (--range) or the source files under --paths.
func RunScope(args []string, stdout, stderr io.Writer) int {
	fs := helptext.NewFlagSet("scope", helptext.Page{
		Usage: "claude-code review scope (--range R | --paths p1,p2) [flags]",
		About: "List the files a change touches (--range sizes each at 80 bytes per changed line) or the " +
			"source files under paths (sized on disk); --importers adds each Go package's in-repo importers.",
	})
	fs.SetOutput(stderr)
	repo := fs.String("repo", ".", "Repository directory")
	diffRange := fs.String("range", "", "Git revision range whose changed files are listed (e.g. HEAD~1..HEAD)")
	paths := fs.String("paths", "", "Comma-separated paths: the pathspec for --range, or the source tree to list")
	importers := fs.Bool("importers", false, "Add each Go package among the files with its in-repo importers")
	format := helptext.FormatFlag(fs)
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	if code := checkScopeArgs(*format, *diffRange, *paths, stderr); code != 0 {
		return code
	}
	root, ok := resolveGitRepo([]string{*repo}, stderr)
	if !ok {
		return 1
	}
	result, err := buildScope(root, *diffRange, splitPaths(*paths), *importers)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	if *format == helptext.JSON {
		return printJSON(stdout, stderr, result)
	}
	return printScopeText(stdout, stderr, result)
}

// checkScopeArgs refuses an unknown format or a call with neither --range nor --paths (exit 2).
func checkScopeArgs(format, diffRange, paths string, stderr io.Writer) int {
	if err := helptext.CheckFormat(format); err != nil {
		fmt.Fprintf(stderr, "claude-code: %v\n", err)
		return 2
	}
	if diffRange == "" && len(splitPaths(paths)) == 0 {
		fmt.Fprintln(stderr, "claude-code: scope needs --range or --paths")
		return 2
	}
	return 0
}

// buildScope lists the files and, with importers, the Go packages among them.
func buildScope(root, diffRange string, paths []string, importers bool) (scopeResult, error) {
	files, err := scopeFiles(root, diffRange, paths)
	if err != nil {
		return scopeResult{}, err
	}
	result := scopeResult{Files: files, Packages: []scopePackage{}}
	if importers {
		if result.Packages, err = scopePackages(root, files); err != nil {
			return scopeResult{}, err
		}
	}
	return result, nil
}

// scopeFiles lists the range's changed files when diffRange is set, else the source files under paths; sorted by path.
func scopeFiles(root, diffRange string, paths []string) ([]scopeFile, error) {
	var files []scopeFile
	var err error
	if diffRange != "" {
		files, err = rangeFiles(root, diffRange, paths)
	} else {
		files, err = sourceFiles(root, paths)
	}
	if err != nil {
		return nil, err
	}
	slices.SortFunc(files, func(a, b scopeFile) int { return strings.Compare(a.Path, b.Path) })
	return files, nil
}

// rangeFiles lists the files changed in diffRange, limited to the pathspec when given, via `git diff --numstat -z`.
func rangeFiles(repo, diffRange string, paths []string) ([]scopeFile, error) {
	if err := checkRefs(diffRange); err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	args := append([]string{"diff", "--numstat", "-z", diffRange, "--"}, paths...)
	out, err := gitInherit(repo, args...)
	if err != nil {
		return nil, err
	}
	return parseNumstat(out), nil
}

// parseNumstat reads `git diff --numstat -z`: "added\tdeleted\tpath\0", or for a rename "added\tdeleted\t\0old\0new\0".
func parseNumstat(out string) []scopeFile {
	files := []scopeFile{}
	tok := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	for i := 0; i < len(tok); i++ {
		fields := strings.SplitN(tok[i], "\t", 3)
		if len(fields) < 3 {
			continue
		}
		name := fields[2]
		if name == "" && i+2 < len(tok) {
			name = tok[i+2]
			i += 2
		}
		files = append(files, scopeFile{Path: name, Bytes: numstatBytes(fields[0], fields[1])})
	}
	return files
}

// numstatBytes estimates a file as (added+deleted) lines * 80 bytes; a binary file's "-" counts as 0 lines.
func numstatBytes(added, deleted string) int64 {
	a, _ := strconv.Atoi(added)
	d, _ := strconv.Atoi(deleted)
	return int64(a+d) * scopeBytesPerLine
}

// sourceFiles lists the source files under each of roots (relative to root), sized by their length on disk.
func sourceFiles(root string, roots []string) ([]scopeFile, error) {
	files := []scopeFile{}
	seen := map[string]bool{}
	for _, p := range roots {
		if underSkipDir(p) {
			continue
		}
		start := filepath.Join(root, p)
		err := filepath.WalkDir(start, func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name != start && scopeSkipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			if !scopeExts[filepath.Ext(name)] {
				return nil
			}
			return addSourceFile(root, name, d, seen, &files)
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

// underSkipDir reports whether a root-relative path has a component named in scopeSkipDirs.
func underSkipDir(p string) bool {
	return slices.ContainsFunc(strings.Split(filepath.ToSlash(filepath.Clean(p)), "/"), func(c string) bool {
		return scopeSkipDirs[c]
	})
}

// addSourceFile appends one source file to files once, keyed by its root-relative slash path.
func addSourceFile(root, name string, d fs.DirEntry, seen map[string]bool, files *[]scopeFile) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, statErr := os.Stat(name)
		if statErr != nil {
			return nil // dangling link: not a listable source file
		}
		info = target
	}
	rel, err := filepath.Rel(root, name)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	if !seen[rel] {
		seen[rel] = true
		*files = append(*files, scopeFile{Path: rel, Bytes: info.Size()})
	}
	return nil
}

// scopePackages lists each Go package dir among files with the files in it and its in-repo importers.
func scopePackages(root string, files []scopeFile) ([]scopePackage, error) {
	byDir := map[string][]string{}
	goDirs := map[string]bool{}
	for _, f := range files {
		dir := path.Dir(f.Path)
		byDir[dir] = append(byDir[dir], f.Path)
		if strings.HasSuffix(f.Path, ".go") {
			goDirs[dir] = true
		}
	}
	pkgs := []scopePackage{}
	if len(goDirs) == 0 {
		return pkgs, nil
	}
	importers, err := goImporters(root)
	if err != nil {
		return nil, err
	}
	for _, dir := range slices.Sorted(maps.Keys(goDirs)) {
		pkgs = append(pkgs, scopePackage{Dir: dir, Files: byDir[dir], Importers: source.NonNil(importers[dir])})
	}
	return pkgs, nil
}

// goImporters maps each in-repo package dir to the sorted in-repo dirs whose packages import it.
func goImporters(root string) (map[string][]string, error) {
	mod, err := modulePath(root)
	if err != nil {
		return nil, err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	out, err := goListPackages(root, realRoot)
	if err != nil {
		return nil, err
	}
	froms, err := scopeImportEdges(out, realRoot, mod)
	if err != nil {
		return nil, err
	}
	importers := make(map[string][]string, len(froms))
	for to, set := range froms {
		importers[to] = slices.Sorted(maps.Keys(set))
	}
	return importers, nil
}

// scopeImportEdges reads `go list` lines ("dir<TAB>imports...") into importee dir -> set of importer dirs.
func scopeImportEdges(out, realRoot, mod string) (map[string]map[string]bool, error) {
	froms := map[string]map[string]bool{}
	for line := range strings.SplitSeq(out, "\n") {
		dir, imports, ok := strings.Cut(line, "\t")
		if !ok || dir == "" {
			continue
		}
		from, err := filepath.Rel(realRoot, dir)
		if err != nil {
			return nil, err
		}
		from = filepath.ToSlash(from)
		for imp := range strings.FieldsSeq(imports) {
			to, ok := inRepoDir(mod, imp)
			if !ok {
				continue
			}
			if froms[to] == nil {
				froms[to] = map[string]bool{}
			}
			froms[to][from] = true
		}
	}
	return froms, nil
}

// inRepoDir maps an import path inside module mod to its repo-relative dir.
func inRepoDir(mod, imp string) (string, bool) {
	switch {
	case imp == mod:
		return ".", true
	case strings.HasPrefix(imp, mod+"/"):
		return strings.TrimPrefix(imp, mod+"/"), true
	}
	return "", false
}

// modulePath reads the module path from root/go.mod: the first "module" line.
func modulePath(root string) (string, error) {
	gomod := filepath.Join(root, "go.mod")
	b, err := os.ReadFile(gomod)
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "module" {
			return strings.Trim(f[1], `"`), nil
		}
	}
	return "", fmt.Errorf("no module line in %s", gomod)
}

// goListPackages runs `go list ./...` in root (realRoot is its symlink-free path): one "dir<TAB>imports..." line each.
func goListPackages(root, realRoot string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-f", "{{.Dir}}\t{{join .Imports \" \"}}", "./...")
	cmd.Dir = root
	// exec sets PWD=root, and go list trusts it: pin it to the real path so {{.Dir}} matches realRoot.
	cmd.Env = append(os.Environ(), "PWD="+realRoot)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// splitPaths splits a comma-separated --paths value, dropping empty entries.
func splitPaths(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// printScopeText writes one "bytes path" line per file, then one "pkg dir importers: ..." line per package.
func printScopeText(stdout, stderr io.Writer, r scopeResult) int {
	for _, f := range r.Files {
		if _, err := fmt.Fprintf(stdout, "%d %s\n", f.Bytes, f.Path); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
	}
	for _, p := range r.Packages {
		if _, err := fmt.Fprintf(stdout, "pkg %s importers: %s\n", p.Dir, importerList(p.Importers)); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return 1
		}
	}
	return 0
}

// importerList joins importer dirs with commas, or "-" when there are none.
func importerList(dirs []string) string {
	if len(dirs) == 0 {
		return "-"
	}
	return strings.Join(dirs, ",")
}
