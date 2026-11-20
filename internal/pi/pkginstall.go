package pi

import (
	"bytes"
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
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// Pinned packages live in one per-agent store, one slot per spec; a run loads a slot with `-e <dir>`.
const (
	specMarker     = ".spec" // written last: a slot without it is a partial install and is removed
	installTimeout = 10 * time.Minute
	walkLimit      = 20_000 // entries a no-code package's link check visits; a bigger package is refused
	walkDepth      = 32     // directory levels the same check descends
)

var (
	resourceKeys = []string{"extensions", "skills", "prompts", "themes"}
	lifecycleKey = []string{"preinstall", "install", "postinstall", "prepare"}
	slotRe       = regexp.MustCompile(`^(npm|git)-[0-9a-f]{16}$`)
	hex40        = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

var _ = schemas.Register(schemas.Format{
	Name: "pi-packages", Owner: "internal/pi", Kind: schemas.Derived,
	Glob:    func() string { return filepath.Join(packageStore(), "*", specMarker) },
	Version: 1, Keep: "pinned package store: `clean` removes a slot the record no longer lists while no pi run is live",
})

// fetchPackage puts p's files in slot (npm: package and deps; git: pinned checkout), scripts off; tests replace it.
var fetchPackage = runFetch

// installDeps installs an approved git package's dependencies in dir; tests replace it.
var installDeps = runDeps

// Review is what an installed package declares, read from its package.json alone.
type Review struct {
	Name, Version                       string
	Extensions, Skills, Prompts, Themes []string
	ShipsCode, HasDeps                  bool
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
	Name, Version  string
	Bin            json.RawMessage
	Scripts        map[string]json.RawMessage
	Pi             map[string]json.RawMessage
	HasPi, HasDeps bool
}

// readManifest decodes package.json by exact key as Node does; Go's struct decoding would fold "PI" into "pi".
func readManifest(dir string) (manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return manifest{}, fmt.Errorf("package review: %w", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return manifest{}, fmt.Errorf("package review: %s/package.json is not a JSON object: %v", dir, err)
	}
	var m manifest
	_ = json.Unmarshal(top["name"], &m.Name) // a non-string name stays "", which no npm pin matches
	_ = json.Unmarshal(top["version"], &m.Version)
	_ = json.Unmarshal(top["scripts"], &m.Scripts)
	var deps map[string]json.RawMessage
	m.HasDeps = json.Unmarshal(top["dependencies"], &deps) == nil && len(deps) > 0
	m.Bin = top["bin"]
	pi, ok := top["pi"]
	if !ok {
		return m, nil
	}
	// fail closed: Pi auto-discovers code when pi is not an object (null, array), so only an object is accepted
	if !bytes.HasPrefix(bytes.TrimSpace(pi), []byte("{")) || json.Unmarshal(pi, &m.Pi) != nil {
		return manifest{}, fmt.Errorf("package review: %s: pi must be an object", dir)
	}
	m.HasPi = true
	return m, nil
}

// ReviewPackage reads dir's package.json; an unreadable or malformed manifest is an error, never "no code".
func ReviewPackage(dir string) (Review, error) {
	m, err := readManifest(dir)
	if err != nil {
		return Review{}, err
	}
	r := Review{Name: m.Name, Version: m.Version, HasDeps: m.HasDeps}
	dst := map[string]*[]string{"extensions": &r.Extensions, "skills": &r.Skills, "prompts": &r.Prompts, "themes": &r.Themes}
	declared := false
	for _, k := range resourceKeys {
		v, ok := m.Pi[k]
		if !ok {
			continue
		}
		declared = true
		// fail closed: pi-manifest.js keeps a field only when it is an array of strings, else Pi auto-discovers
		if !bytes.HasPrefix(bytes.TrimSpace(v), []byte("[")) || json.Unmarshal(v, dst[k]) != nil || slices.ContainsFunc(*dst[k], escapes) {
			return Review{}, fmt.Errorf("package review: %s: pi.%s must be an array of paths inside the package", dir, k)
		}
	}
	r.CodeReasons = codeReasons(dir, m, r, declared)
	r.ShipsCode = len(r.CodeReasons) > 0
	if !r.ShipsCode { // approved code may do anything; a no-code package must not reach outside itself
		if err := checkLinks(dir); err != nil {
			return Review{}, err
		}
	}
	return r, nil
}

// escapes is true for an absolute entry or one with a .. segment: Pi would resolve it outside the package.
func escapes(p string) bool {
	seg := strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/") || slices.Contains(seg, "..")
}

// checkLinks refuses a symlink whose target leaves the package: Pi follows links when it collects resource files.
func checkLinks(dir string) error {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("package review: %w", err)
	}
	n := 0
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		// fail closed: a tree too big or too deep to check is refused, never loaded unchecked
		if n++; n > walkLimit || strings.Count(rel, string(filepath.Separator)) > walkDepth {
			return fmt.Errorf("package review: %s is over %d entries or %d levels deep; refused", dir, walkLimit, walkDepth)
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		// fail closed: a dangling link or one resolving outside the package is refused
		if t, err := filepath.EvalSymlinks(p); err != nil || !paths.Within(t, root) {
			return fmt.Errorf("package review: %s: symlink %s points outside the package", dir, rel)
		}
		return nil
	})
}

// autoCode is what Pi's collectPackageResources finds with no manifest: extensions/, else the dir itself as one.
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

func packageStore() string { return storeUnder(paths.StateDir()) }

func storeUnder(root string) string { return filepath.Join(root, "pi", "packages") }

// checkStore refuses a store whose real path is not strictly inside root's real path (a symlinked store or pi dir).
func checkStore(root, store string) error {
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("package store: %w", err)
	}
	s, err := filepath.EvalSymlinks(store)
	if err != nil {
		return fmt.Errorf("package store: %w", err)
	}
	if s == r || !paths.Within(s, r) {
		return fmt.Errorf("package store: %s resolves to %s, outside %s; refusing to install or remove", store, s, r)
	}
	return nil
}

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

// KeepSlot is clean's keep test: false only for a slot the Pi record no longer lists, in a store inside the state dir.
func KeepSlot(path string) bool {
	rec, err := agentconfig.Settings(agentid.Pi)
	if err != nil {
		return true // fail closed: an unreadable record keeps every slot
	}
	wanted, err := ParsePackages(rec.Extras[extraPackages])
	return err != nil || keepSlotIn(paths.StateDir(), path, wanted)
}

// keepSlotIn keeps anything that is not a real slot directly in a store whose real path sits inside root's.
func keepSlotIn(root, path string, wanted []Package) bool {
	store := storeUnder(root)
	if filepath.Dir(filepath.Clean(path)) != store || !slotRe.MatchString(filepath.Base(path)) || checkStore(root, store) != nil {
		return true
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&fs.ModeSymlink != 0 {
		return true
	}
	return slices.ContainsFunc(wanted, func(p Package) bool { return slotName(p) == filepath.Base(path) })
}

// SyncPackages installs missing pins and returns the approved ones (error: no launch); `clean` prunes, never a launch.
func SyncPackages(ctx context.Context, w io.Writer, configDir string, wanted, approved []Package) ([]Loaded, error) {
	return syncIn(ctx, w, paths.StateDir(), configDir, wanted, approved)
}

// syncer is one sync's fixed inputs.
type syncer struct {
	w                io.Writer
	store, configDir string
	approved         []Package
}

func syncIn(ctx context.Context, w io.Writer, root, configDir string, wanted, approved []Package) ([]Loaded, error) {
	if len(wanted) == 0 {
		return nil, nil // nothing pinned touches no store, lock or install
	}
	store := storeUnder(root)
	if err := os.MkdirAll(store, 0o700); err != nil {
		return nil, err
	}
	if err := checkStore(root, store); err != nil {
		return nil, err
	}
	var out []Loaded
	sy := syncer{w, store, configDir, approved}
	err := paths.WithFileLock(filepath.Join(store, "sync"), func() error {
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

// one reviews p every time; a complete slot failing a check stays for clean (a live run may load it) but never loads.
func (sy syncer) one(ctx context.Context, p Package) (Loaded, error) {
	if p.Kind == KindLocal {
		return sy.review(p, p.LocalPath(sy.configDir))
	}
	slot := filepath.Join(sy.store, slotName(p))
	if b, err := os.ReadFile(filepath.Join(slot, specMarker)); err == nil && string(b) == p.Spec {
		return sy.review(p, packageDir(slot, p))
	}
	l, err := sy.install(ctx, p, slot)
	if err != nil {
		return Loaded{}, errors.Join(err, removeSlot(sy.store, slot)) // a fresh slot no run has loaded
	}
	return l, nil
}

// review checks the package at dir against its pin and the approvals.
func (sy syncer) review(p Package, dir string) (Loaded, error) {
	rv, err := ReviewPackage(dir)
	if err != nil {
		return Loaded{}, fmt.Errorf("%s: %w", p.Spec, err)
	}
	if err = checkPin(p, dir, rv); err != nil {
		return Loaded{}, err
	}
	if rv.ShipsCode && !Approved(p.Spec, sy.approved) {
		return Loaded{}, unapprovedError(p, rv, sy.approved)
	}
	if p.Kind == KindNPM {
		rv.Resolved, rv.Integrity = lockEntry(filepath.Join(sy.store, slotName(p)), p.Name)
	}
	return Loaded{Pkg: p, Dir: dir, Review: rv}, nil
}

// install fetches p into a fresh slot and reviews it; only then does an approved git package get its dependencies.
func (sy syncer) install(ctx context.Context, p Package, slot string) (Loaded, error) {
	if err := removeSlot(sy.store, slot); err != nil { // a partial slot from a crashed install
		return Loaded{}, err
	}
	if err := os.Mkdir(slot, 0o700); err != nil {
		return Loaded{}, err
	}
	fmt.Fprintf(sy.w, "pi packages: installing %s\n", p.Spec)
	env := installEnv(ctx)
	if err := fetchPackage(ctx, sy.w, p, slot, env); err != nil {
		return Loaded{}, fmt.Errorf("install %s: %w", p.Spec, err)
	}
	l, err := sy.review(p, packageDir(slot, p))
	if err != nil {
		return Loaded{}, err
	}
	if p.Kind == KindGit && l.Review.ShipsCode && l.Review.HasDeps { // reviewed and approved: code with dependencies
		if err := installDeps(ctx, sy.w, l.Dir, env); err != nil {
			return Loaded{}, fmt.Errorf("install %s dependencies: %w", p.Spec, err)
		}
	}
	return l, os.WriteFile(filepath.Join(slot, specMarker), []byte(p.Spec), 0o600)
}

// checkPin refuses an installed package whose name, version or commit is not exactly the pin.
func checkPin(p Package, dir string, rv Review) error {
	switch p.Kind {
	case KindNPM:
		if rv.Name != p.Name || rv.Version != p.Version {
			return fmt.Errorf("%s: installed %s@%s does not match the pin", p.Spec, rv.Name, rv.Version)
		}
	case KindGit: // a detached HEAD file holds the checked-out commit: the rev-parse HEAD check without a spawn
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

// installEnv is the installer's env: the credential scrub, no inherited npm or git config, scripts and prompts off.
func installEnv(ctx context.Context) []string {
	env := slices.DeleteFunc(agent.EnvSpec{Mode: agent.EnvAgent}.Environ(agent.ProbeContext(ctx, "")), func(kv string) bool {
		k := strings.ToUpper(kv[:max(strings.IndexByte(kv, '='), 0)])
		return strings.HasPrefix(k, "NPM_CONFIG_") || strings.HasPrefix(k, "GIT_")
	})
	return append(env, "npm_config_ignore_scripts=true", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
}

// npmFlags keep npm off lifecycle scripts and every user or global npmrc; the flag beats any env or project config.
var npmFlags = []string{"--ignore-scripts", "--userconfig=" + os.DevNull, "--globalconfig=" + os.DevNull,
	"--legacy-peer-deps", "--no-audit", "--no-fund"}

// npmInstallArgs mirrors Pi's own managed install under npmFlags; run from the slot, which holds no .npmrc.
func npmInstallArgs(specs []string, prefix string) []string {
	return append(append(append([]string{"install"}, specs...), "--prefix", prefix), npmFlags...)
}

// gitArgv is one git command with repo hooks, fsmonitor, file/ext transports, symlinks and submodules off.
func gitArgv(args ...string) []string {
	return append([]string{"git", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false",
		"-c", "protocol.file.allow=never", "-c", "protocol.ext.allow=never", "-c", "core.symlinks=false",
		"-c", "submodule.recurse=false"}, args...)
}

func runFetch(ctx context.Context, w io.Writer, p Package, slot string, env []string) error {
	if p.Kind == KindGit {
		return fetchGit(ctx, w, p, packageDir(slot, p), env)
	}
	if err := os.WriteFile(filepath.Join(slot, "package.json"), []byte(`{"name":"pi-extensions","private":true}`), 0o600); err != nil {
		return err
	}
	return runCmd(ctx, w, slot, env, append([]string{"npm"}, npmInstallArgs([]string{p.Name + "@" + p.Version}, slot)...)...)
}

// fetchGit checks out the pinned commit: a shallow fetch of it, else the branch heads; no tags or submodules.
func fetchGit(ctx context.Context, w io.Writer, p Package, repo string, env []string) error {
	url := "https://" + p.Name
	if err := runCmd(ctx, w, filepath.Dir(repo), env, gitArgv("init", "-q", "--template=", repo)...); err != nil {
		return err
	}
	fetch := []string{"fetch", "-q", "--no-tags", "--no-recurse-submodules"}
	if err := runCmd(ctx, w, repo, env, gitArgv(append(fetch, "--depth=1", url, p.Version)...)...); err != nil {
		if err := runCmd(ctx, w, repo, env, gitArgv(append(fetch, url, "+refs/heads/*:refs/remotes/origin/*")...)...); err != nil {
			return err
		}
	}
	return runCmd(ctx, w, repo, env, gitArgv("checkout", "-q", "--detach", p.Version)...)
}

// runDeps installs a reviewed, approved git package's dependencies; fail closed: a repo .npmrc would configure npm.
func runDeps(ctx context.Context, w io.Writer, dir string, env []string) error {
	if _, err := os.Lstat(filepath.Join(dir, ".npmrc")); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s ships an .npmrc; the harness never installs dependencies under a repo's own npm config", dir)
	}
	return runCmd(ctx, w, dir, env, append([]string{"npm", "install", "--omit=dev"}, npmFlags...)...)
}

func runCmd(ctx context.Context, w io.Writer, dir string, env []string, argv ...string) error {
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, w, w
	return cmd.Run()
}
