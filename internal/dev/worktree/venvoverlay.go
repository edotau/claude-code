package worktree

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// venvBuildTimeout bounds creating a lane's overlay venv.
const venvBuildTimeout = 2 * time.Minute

var (
	mappingStart = regexp.MustCompile(`(?m)^MAPPING\b[^=\n]*=\s*\{`)
	// mappingPair is one 'name': 'path' pair, either quote style, of a setuptools editable finder's MAPPING table.
	mappingPair = regexp.MustCompile(`(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)")\s*:\s*(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)")`)
	unescapePy  = strings.NewReplacer(`\\`, `\`, `\'`, `'`, `\"`, `"`)
	// envPython is a `#!/usr/bin/env python[3[.x]]` shebang start, which PATH would resolve outside the lane venv.
	envPython = regexp.MustCompile(`^#!\s*/usr/bin/env\s+(-S\s+)?python(?:3(?:\.[0-9]+)?)?(\s|$)`)
)

// overlayVenv builds the lane's own venv at target that reuses the primary venv's packages but imports lane source.
func overlayVenv(source, target, lane string) error {
	if err := createVenv(source, target); err != nil {
		return errors.Join(err, os.RemoveAll(target))
	}
	if err := errors.Join(writeOverlayPth(source, target, lane), copyScripts(source, target), inheritSystemSitePackages(source, target)); err != nil {
		return errors.Join(err, os.RemoveAll(target))
	}
	return nil
}

// createVenv makes an empty venv at target with the primary's Python minor version: uv, else the primary's python.
func createVenv(source, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), venvBuildTimeout)
	defer cancel()
	var uvErr error
	if uv, err := exec.LookPath("uv"); err == nil && pyvenvVersion(source) != "" {
		if uvErr = runQuiet(exec.CommandContext(ctx, uv, "venv", "--quiet", "--python", pyvenvVersion(source), target)); uvErr == nil {
			return nil
		}
		_ = os.RemoveAll(target)
	}
	py := filepath.Join(source, "bin", "python")
	if err := runQuiet(exec.CommandContext(ctx, py, "-S", "-m", "venv", "--without-pip", target)); err != nil {
		return errors.Join(uvErr, err)
	}
	return nil
}

func runQuiet(cmd *exec.Cmd) error {
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", filepath.Base(cmd.Path), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// pyvenvVersion is the major.minor Python version in venv's pyvenv.cfg ("version_info" or uv's "version"), else "".
func pyvenvVersion(venv string) string {
	f, err := os.Open(filepath.Join(venv, "pyvenv.cfg"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if k = strings.TrimSpace(k); ok && (k == "version_info" || k == "version") {
			parts := strings.Split(strings.TrimSpace(v), ".")
			if len(parts) >= 2 {
				return parts[0] + "." + parts[1]
			}
		}
	}
	_ = sc.Err() // a read error leaves the version unknown, as a missing key does
	return ""
}

// sitePackages is venv's site-packages directory (lib/pythonX.Y on unix, Lib on Windows), "" when absent.
func sitePackages(venv string) string {
	for _, pattern := range []string{"lib/python*/site-packages", "Lib/site-packages"} {
		if m := globFiles(venv, pattern); len(m) > 0 {
			return m[0]
		}
	}
	return ""
}

func globFiles(dir, pattern string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, pattern))
	return m
}

// writeOverlayPth points the lane venv at the primary's packages and at the primary's editable roots rerooted to lane.
func writeOverlayPth(source, target, lane string) error {
	psp, lsp := sitePackages(source), sitePackages(target)
	if psp == "" || lsp == "" {
		return errors.New("venv has no site-packages")
	}
	roots, links, err := editableRoots(psp, filepath.Dir(source), lane)
	if err != nil {
		return err
	}
	// a .pth line is a path per row: a name with a line break could smuggle an executable `import` row
	if slices.ContainsFunc(append([]string{psp}, roots...), func(p string) bool { return strings.ContainsAny(p, "\r\n") }) {
		return errors.New("a path with a line break cannot go in a .pth file")
	}
	// a path line does not run the primary's own .pth files, so its editable finders never load
	if err := os.WriteFile(filepath.Join(lsp, "b-primary-deps.pth"), []byte(psp+"\n"), 0o644); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(links)) {
		if err := linkMapping(lsp, filepath.Join(lsp, name), links[name]); err != nil {
			return err
		}
	}
	// bytecode of the shared packages must not be written into the primary venv
	cache := fmt.Sprintf("import sys; sys.pycache_prefix = %q\n", filepath.Join(target, "pycache"))
	body := cache + nspkgLines(psp) + dropPreloaded(links) + strings.Join(roots, "\n") + "\n"
	return os.WriteFile(filepath.Join(lsp, "a-lane-roots.pth"), []byte(body), 0o644)
}

// linkMapping links one editable mapping under lsp; a mapped `__init__.py` replaces the synthetic one.
func linkMapping(lsp, link, target string) error {
	if err := makeSyntheticParents(lsp, filepath.Dir(link)); err != nil {
		return err
	}
	if filepath.Base(link) == "__init__.py" {
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	return os.Symlink(target, link)
}

// makeSyntheticParents makes dir's levels regular extend_path packages; a bare namespace dir loses to a regular one.
func makeSyntheticParents(root, dir string) error {
	const initBody = "__path__ = __import__('pkgutil').extend_path(__path__, __name__)\n"
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." {
		return err
	}
	cur := root
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("editable mappings overlap at %s", cur)
		}
		if err := os.MkdirAll(cur, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(cur, "__init__.py"), []byte(initBody), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// dropPreloaded is a .pth row that unloads a namespace (and its children) a primary *-nspkg.pth preloaded.
func dropPreloaded(links map[string]string) string {
	var parents []string
	for name := range links {
		parts := strings.Split(filepath.Dir(name), string(filepath.Separator))
		for i := range parts {
			if parts[i] != "." {
				parents = append(parents, strconv.Quote(strings.Join(parts[:i+1], ".")))
			}
		}
	}
	if len(parents) == 0 {
		return ""
	}
	slices.Sort(parents)
	p := "(" + strings.Join(slices.Compact(parents), ", ") + ",)"
	return "import sys; [sys.modules.pop(n, None) for n in list(sys.modules) if n in " + p +
		" or n.startswith(tuple(q + '.' for q in " + p + "))]\n"
}

// nspkgLines are the primary's *-nspkg.pth rows, with the site dir they read from pinned to the primary's.
func nspkgLines(psp string) string {
	var out strings.Builder
	for _, pth := range globFiles(psp, "*-nspkg.pth") {
		data, _ := os.ReadFile(pth)
		for line := range strings.SplitSeq(string(data), "\n") {
			if strings.HasPrefix(line, "import") {
				out.WriteString(strings.ReplaceAll(line, "sys._getframe(1).f_locals['sitedir']", strconv.Quote(psp)))
				out.WriteByte('\n')
			}
		}
	}
	return out.String()
}

// copyScripts gives the lane venv the primary's bin entries: scripts re-pointed at the lane python, others linked.
func copyScripts(source, target string) error {
	entries, err := os.ReadDir(filepath.Join(source, "bin"))
	if err != nil {
		return err
	}
	sep, laneBin := string(filepath.Separator), filepath.Join(target, "bin")+string(filepath.Separator)
	pairs := []string{filepath.Join(source, "bin") + sep, laneBin}
	if real, err := filepath.EvalSymlinks(source); err == nil {
		pairs = append(pairs, filepath.Join(real, "bin")+sep, laneBin)
	}
	rep := strings.NewReplacer(pairs...)
	for _, e := range entries {
		dst := filepath.Join(target, "bin", e.Name())
		if _, err := os.Lstat(dst); err == nil || e.IsDir() {
			continue // python and activate come with the new venv
		}
		if err := copyScript(filepath.Join(source, "bin", e.Name()), dst, filepath.Join(laneBin, "python"), rep); err != nil {
			return err
		}
	}
	return nil
}

// copyScript writes dst as src with the primary bin path in its first three rows swapped, or links a non-script.
func copyScript(src, dst, lanePy string, rep *strings.Replacer) error {
	f, err := os.Open(src)
	if err != nil {
		return os.Symlink(src, dst)
	}
	magic := make([]byte, 2)
	_, err = io.ReadFull(f, magic)
	_ = f.Close()
	if err != nil || string(magic) != "#!" {
		return os.Symlink(src, dst)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	cut := len(data)
	for n, i := 0, 0; i < len(data); i++ {
		if data[i] == '\n' {
			if n++; n == 3 {
				cut = i
				break
			}
		}
	}
	head := laneShebang(rep.Replace(string(data[:cut])), lanePy)
	return os.WriteFile(dst, append([]byte(head), data[cut:]...), 0o755)
}

// laneShebang points an `env python` shebang at lanePy, keeping `env -S`; an unsafe path leaves the row alone.
func laneShebang(head, lanePy string) string {
	m := envPython.FindStringSubmatchIndex(head)
	if m == nil || strings.ContainsAny(lanePy, " \t$\"'\\`") {
		return head
	}
	prefix := "#!"
	if m[2] >= 0 {
		prefix = "#!/usr/bin/env -S "
	}
	return prefix + lanePy + head[m[4]:]
}

// editableRoots mirrors the primary's editable installs (path .pth lines, finder MAPPING tables) under lane.
func editableRoots(sp, primary, lane string) (roots []string, links map[string]string, err error) {
	reroot := rerooter(primary, lane)
	if roots, err = pthRoots(sp, reroot); err != nil {
		return nil, nil, err
	}
	links = map[string]string{}
	for _, finder := range globFiles(sp, "__editable__*finder.py") {
		data, err := os.ReadFile(finder)
		if err != nil {
			return nil, nil, err
		}
		for name, path := range parseMapping(string(data)) {
			target := reroot(path)
			switch {
			case target == "":
			case filepath.Base(target) == name || filepath.Base(target) == name+".py":
				roots = append(roots, filepath.Dir(target))
			default:
				links[strings.ReplaceAll(name, ".", string(filepath.Separator))+filepath.Ext(target)] = target
			}
		}
	}
	return slices.Compact(slices.Sorted(slices.Values(roots))), links, nil
}

// pthRoots are the rerooted directory lines of every .pth file in sp (import lines are skipped).
func pthRoots(sp string, reroot func(string) string) ([]string, error) {
	var roots []string
	for _, pth := range globFiles(sp, "*.pth") {
		data, err := os.ReadFile(pth)
		if err != nil {
			return nil, err
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSpace(line)
			if foreignEditable(line) {
				return nil, fmt.Errorf("%s: editable finder this build cannot reroot", filepath.Base(pth))
			}
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "import") {
				continue
			}
			if !filepath.IsAbs(line) {
				line = filepath.Join(sp, line)
			}
			if q := reroot(line); q != "" {
				roots = append(roots, q)
			}
		}
	}
	return roots, nil
}

// foreignEditable reports a .pth import row of an editable finder other than setuptools', which this build cannot map.
func foreignEditable(line string) bool {
	return strings.HasPrefix(line, "import") && strings.Contains(strings.ToLower(line), "editable") && !strings.Contains(line, "__editable__")
}

// parseMapping reads the name -> path pairs of a setuptools editable finder's MAPPING table, across lines.
func parseMapping(src string) map[string]string {
	loc := mappingStart.FindStringIndex(src)
	if loc == nil {
		return nil
	}
	pairs := map[string]string{}
	for _, m := range mappingPair.FindAllStringSubmatch(mappingBody(src[loc[1]:]), -1) {
		pairs[unescapePy.Replace(m[1]+m[2])] = unescapePy.Replace(m[3] + m[4])
	}
	return pairs
}

// mappingBody is the text up to the closing brace of a dict literal, skipping braces inside string literals.
func mappingBody(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		if quote != 0 {
			switch s[i] {
			case '\\':
				i++
			case quote:
				quote = 0
			}
			continue
		}
		switch s[i] {
		case '\'', '"':
			quote = s[i]
		case '}':
			return s[:i]
		}
	}
	return s
}

// rerooter maps a path under primary to the same path under lane ("" when the lane lacks it); other paths pass through.
func rerooter(primary, lane string) func(string) string {
	roots := []string{primary}
	if real, err := filepath.EvalSymlinks(primary); err == nil {
		roots = append(roots, real)
	}
	return func(p string) string {
		p = filepath.Clean(p)
		for _, root := range roots {
			if rel, err := filepath.Rel(root, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				q := filepath.Join(lane, rel)
				if _, err := os.Stat(q); err != nil && len(globFiles(filepath.Dir(q), filepath.Base(q)+".*")) == 0 {
					return ""
				}
				return q
			}
		}
		return p
	}
}

// inheritSystemSitePackages copies the primary's include-system-site-packages = true into the lane venv's pyvenv.cfg.
func inheritSystemSitePackages(source, target string) error {
	const off, on = "include-system-site-packages = false", "include-system-site-packages = true"
	primary, err := os.ReadFile(filepath.Join(source, "pyvenv.cfg"))
	if err != nil || !strings.Contains(string(primary), on) {
		return nil
	}
	cfg := filepath.Join(target, "pyvenv.cfg")
	data, err := os.ReadFile(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(cfg, []byte(strings.Replace(string(data), off, on, 1)), 0o644)
}
