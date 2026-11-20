package pi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// Pinned packages live in one per-agent store, one slot per spec; a run loads a slot with `-e <dir>`.
const (
	specMarker     = ".spec" // written last: a slot without it is a partial install and is removed
	installTimeout = 10 * time.Minute
)

var (
	resourceKeys = []string{"extensions", "skills", "prompts", "themes"}
	lifecycleKey = []string{"preinstall", "install", "postinstall", "prepare"}
	slotRe       = regexp.MustCompile(`^(npm|git)-[0-9a-f]{16}$`)
	hex40        = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

var _ = schemas.Register(schemas.Format{
	Name: "pi-packages", Owner: "internal/pi", Kind: schemas.Derived,
	Glob:    func() string { return filepath.Join(paths.StateDir(), "pi", "packages", "*", specMarker) },
	Version: 1, Keep: "pinned package store: the next sync removes any slot the record no longer lists",
})

// installPackage fills slot with p; tests replace it. Lifecycle scripts must stay off whatever it does.
var installPackage = runInstaller

// Review is what an installed package declares, read from its package.json alone.
type Review struct {
	Name, Version                       string
	Extensions, Skills, Prompts, Themes []string
	ShipsCode                           bool
	CodeReasons                         []string
	Resolved, Integrity                 string // npm lockfile fields, informational
}

// Loaded is one reviewed package a run loads: Dir goes to Pi's -e.
type Loaded struct {
	Pkg    Package
	Dir    string
	Review Review
}

type manifest struct {
	Name    string                     `json:"name"`
	Version string                     `json:"version"`
	Bin     json.RawMessage            `json:"bin"`
	Scripts map[string]json.RawMessage `json:"scripts"`
	Pi      map[string]json.RawMessage `json:"pi"`
}

// ReviewPackage reads dir's package.json; an unreadable or malformed manifest is an error, never "no code".
func ReviewPackage(dir string) (Review, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return Review{}, fmt.Errorf("package review: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Review{}, fmt.Errorf("package review: %s/package.json: %w", dir, err)
	}
	r := Review{Name: m.Name, Version: m.Version}
	dst := map[string]*[]string{"extensions": &r.Extensions, "skills": &r.Skills, "prompts": &r.Prompts, "themes": &r.Themes}
	declared := false
	for _, k := range resourceKeys {
		v, ok := m.Pi[k]
		if !ok {
			continue
		}
		declared = true
		if err := json.Unmarshal(v, dst[k]); err != nil || slices.ContainsFunc(*dst[k], escapes) {
			return Review{}, fmt.Errorf("package review: %s: pi.%s must be an array of paths inside the package", dir, k)
		}
	}
	r.CodeReasons = codeReasons(dir, m, r, declared)
	r.ShipsCode = len(r.CodeReasons) > 0
	return r, nil
}

// escapes is true for an absolute entry or one with a .. segment: Pi would resolve it outside the package.
func escapes(p string) bool {
	seg := strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/") || slices.Contains(seg, "..")
}

// autoCode is the code Pi finds without a pi manifest: an extensions/ dir, or the package dir itself when none exist.
func autoCode(dir string) []string {
	has := func(k string) bool { _, err := os.Stat(filepath.Join(dir, k)); return err == nil }
	if has("extensions") {
		return []string{"extensions/ directory (auto-discovered)"}
	}
	if !slices.ContainsFunc(resourceKeys, has) {
		return []string{"no resource directories: Pi loads the package directory itself as an extension"}
	}
	return nil
}

// codeReasons lists everything in the package that can execute: extensions, bin entries and npm lifecycle scripts.
func codeReasons(dir string, m manifest, r Review, declared bool) []string {
	var out []string
	if len(r.Extensions) > 0 {
		out = append(out, "pi.extensions")
	}
	if !declared {
		out = append(out, autoCode(dir)...)
	}
	if b := strings.TrimSpace(string(m.Bin)); b != "" && b != "null" && b != `""` && b != "{}" {
		out = append(out, "bin")
	}
	for _, k := range lifecycleKey {
		if _, ok := m.Scripts[k]; ok {
			out = append(out, "npm script "+k+" (never run by the harness)")
		}
	}
	return out
}

// String renders the review for the approval message.
func (r Review) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "  package %s@%s\n  extensions=%v skills=%v prompts=%v themes=%v\n", r.Name, r.Version, r.Extensions, r.Skills, r.Prompts, r.Themes)
	fmt.Fprintf(&b, "  ships code: %v %v", r.ShipsCode, r.CodeReasons)
	if r.Integrity != "" {
		fmt.Fprintf(&b, "\n  resolved %s integrity %s", r.Resolved, r.Integrity)
	}
	return b.String()
}

// PackageArgv is one `-e <dir>` per package (lockdown flags leave -e working); put it after the guard's own -e.
func PackageArgv(loaded []Loaded) []string {
	var argv []string
	for _, l := range loaded {
		argv = append(argv, "-e", l.Dir)
	}
	return argv
}

func packageStore() string { return filepath.Join(paths.StateDir(), "pi", "packages") }

func slotName(p Package) string {
	sum := sha256.Sum256([]byte(p.Spec))
	return string(p.Kind) + "-" + hex.EncodeToString(sum[:8])
}

// packageDir is where an installed p's package.json lives inside slot (the layout Pi's own installer uses).
func packageDir(slot string, p Package) string {
	if p.Kind == KindGit {
		return filepath.Join(slot, "repo")
	}
	return filepath.Join(slot, "node_modules", filepath.FromSlash(p.Name))
}

// removeSlot deletes one slot and refuses anything that is not a direct child of store named like a slot.
func removeSlot(store, slot string) error {
	store, slot = filepath.Clean(store), filepath.Clean(slot)
	home, _ := os.UserHomeDir()
	if !filepath.IsAbs(store) || store == filepath.Dir(store) || store == filepath.Clean(home) {
		return fmt.Errorf("package store: refusing to remove under %q", store)
	}
	if filepath.Dir(slot) != store || !slotRe.MatchString(filepath.Base(slot)) {
		return fmt.Errorf("package store: refusing to remove %q: not a slot of %q", slot, store)
	}
	if fi, err := os.Lstat(slot); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("package store: refusing to remove symlink %q", slot)
	}
	return os.RemoveAll(slot)
}

// SyncPackages leaves the store holding exactly the reviewed, approved pins; any error means do not launch.
func SyncPackages(ctx context.Context, w io.Writer, configDir string, wanted, approved []Package) ([]Loaded, error) {
	return syncIn(ctx, w, packageStore(), configDir, wanted, approved)
}

// syncer is one sync's fixed inputs.
type syncer struct {
	w                io.Writer
	store, configDir string
	approved         []Package
}

func syncIn(ctx context.Context, w io.Writer, store, configDir string, wanted, approved []Package) ([]Loaded, error) {
	if _, err := os.Stat(store); errors.Is(err, fs.ErrNotExist) && len(wanted) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(store, 0o700); err != nil {
		return nil, err
	}
	var out []Loaded
	sy := syncer{w, store, configDir, approved}
	err := paths.WithFileLock(filepath.Join(store, "sync"), func() error {
		if err := pruneStore(w, store, wanted); err != nil {
			return err
		}
		for _, p := range wanted {
			l, err := sy.one(ctx, p)
			if err != nil {
				return err
			}
			out = append(out, l)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// pruneStore removes every slot no wanted pin owns.
func pruneStore(w io.Writer, store string, wanted []Package) error {
	keep := map[string]bool{}
	for _, p := range wanted {
		keep[slotName(p)] = true
	}
	ents, err := os.ReadDir(store)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !slotRe.MatchString(e.Name()) || keep[e.Name()] {
			continue
		}
		slot := filepath.Join(store, e.Name())
		spec, _ := os.ReadFile(filepath.Join(slot, specMarker))
		if err := removeSlot(store, slot); err != nil {
			return err
		}
		fmt.Fprintf(w, "pi packages: removed %s\n", strings.TrimSpace(string(spec))+" ("+e.Name()+")")
	}
	return nil
}

// one installs p if missing, reviews it every time; a managed slot failing any check is removed (fail closed).
func (sy syncer) one(ctx context.Context, p Package) (l Loaded, err error) {
	dir, slot := p.LocalPath(sy.configDir), ""
	if p.Kind != KindLocal {
		slot = filepath.Join(sy.store, slotName(p))
		defer func() {
			if err != nil {
				err = errors.Join(err, removeSlot(sy.store, slot))
			}
		}()
		if err = ensureInstalled(ctx, sy.w, slot, p); err != nil {
			return l, err
		}
		dir = packageDir(slot, p)
	}
	rv, err := ReviewPackage(dir)
	if err != nil {
		return l, fmt.Errorf("%s: %w", p.Spec, err)
	}
	if err = checkPin(p, dir, rv); err != nil {
		return l, err
	}
	if rv.ShipsCode && !Approved(p.Spec, sy.approved) {
		return l, unapprovedError(p, rv, sy.approved)
	}
	if slot != "" {
		rv.Resolved, rv.Integrity = lockEntry(slot, p.Name)
	}
	return Loaded{Pkg: p, Dir: dir, Review: rv}, nil
}

// ensureInstalled does nothing (no output, no network) when slot is complete for exactly p.Spec.
func ensureInstalled(ctx context.Context, w io.Writer, slot string, p Package) error {
	if b, err := os.ReadFile(filepath.Join(slot, specMarker)); err == nil && string(b) == p.Spec {
		return nil
	}
	store := filepath.Dir(slot)
	if err := removeSlot(store, slot); err != nil { // a partial slot from a crashed install
		return err
	}
	if err := os.Mkdir(slot, 0o700); err != nil {
		return err
	}
	fmt.Fprintf(w, "pi packages: installing %s\n", p.Spec)
	if err := installPackage(ctx, w, p, slot, installEnv(ctx)); err != nil {
		return fmt.Errorf("install %s: %w", p.Spec, err)
	}
	return os.WriteFile(filepath.Join(slot, specMarker), []byte(p.Spec), 0o600)
}

// checkPin refuses an installed package whose name, version or commit is not exactly the pin.
func checkPin(p Package, dir string, rv Review) error {
	switch p.Kind {
	case KindNPM:
		if rv.Name != p.Name || rv.Version != p.Version {
			return fmt.Errorf("%s: installed %s@%s does not match the pin", p.Spec, rv.Name, rv.Version)
		}
	case KindGit:
		head, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD"))
		if got := strings.TrimSpace(string(head)); err != nil || !hex40.MatchString(got) || got != p.Version {
			return fmt.Errorf("%s: checked-out commit %q does not match the pin", p.Spec, got)
		}
	}
	return nil
}

func unapprovedError(p Package, rv Review, approved []Package) error {
	specs := []string{}
	for _, a := range approved {
		specs = append(specs, a.Spec)
	}
	specs = append(specs, p.Spec)
	return fmt.Errorf("pi package %s ships code and is not approved; review:\n%s\napprove it with: claude-code configure agents pi packages_approved=%s",
		p.Spec, rv, strings.Join(specs, ","))
}

// lockEntry reads the npm lockfile's resolved URL and integrity for name; absent fields are empty.
func lockEntry(root, name string) (resolved, integrity string) {
	var lock struct {
		Packages map[string]struct{ Resolved, Integrity string } `json:"packages"`
	}
	if b, err := os.ReadFile(filepath.Join(root, "package-lock.json")); err == nil && json.Unmarshal(b, &lock) == nil {
		e := lock.Packages["node_modules/"+name]
		return e.Resolved, e.Integrity
	}
	return "", ""
}

// installEnv is the installer's env: no credentials (the vendor-installer scrub) and npm lifecycle scripts off.
func installEnv(ctx context.Context) []string {
	return agent.EnvSpec{Mode: agent.EnvAgent, Set: []string{"npm_config_ignore_scripts=true", "GIT_TERMINAL_PROMPT=0"}}.
		Environ(agent.ProbeContext(ctx, ""))
}

// npmInstallArgs mirrors Pi's own managed install, plus --ignore-scripts as the flag, which beats any env or .npmrc.
func npmInstallArgs(specs []string, prefix string) []string {
	return append(append([]string{"install"}, specs...), "--prefix", prefix, "--ignore-scripts", "--legacy-peer-deps", "--no-audit", "--no-fund")
}

func runInstaller(ctx context.Context, w io.Writer, p Package, slot string, env []string) error {
	if p.Kind == KindGit {
		repo := packageDir(slot, p)
		if err := runCmd(ctx, w, slot, env, "git", "clone", "--no-checkout", "https://"+p.Name, repo); err != nil {
			return err
		}
		if err := runCmd(ctx, w, repo, env, "git", "checkout", "--detach", p.Version); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(repo, "package.json")); err != nil {
			return nil
		}
		return runCmd(ctx, w, repo, env, "npm", "install", "--omit=dev", "--ignore-scripts", "--legacy-peer-deps", "--no-audit", "--no-fund")
	}
	if err := os.WriteFile(filepath.Join(slot, "package.json"), []byte(`{"name":"pi-extensions","private":true}`), 0o600); err != nil {
		return err
	}
	return runCmd(ctx, w, slot, env, append([]string{"npm"}, npmInstallArgs([]string{p.Name + "@" + p.Version}, slot)...)...)
}

func runCmd(ctx context.Context, w io.Writer, dir string, env []string, argv ...string) error {
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, w, w
	return cmd.Run()
}
