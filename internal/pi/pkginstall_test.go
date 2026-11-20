package pi

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReviewPackage(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		code  bool
		fail  bool
	}{
		{"skills only", map[string]string{"package.json": `{"name":"a","version":"1.0.0","pi":{"skills":["./skills"]}}`}, false, false},
		{"conventional skills dir", map[string]string{"package.json": `{"name":"a"}`, "skills/s/SKILL.md": "x"}, false, false},
		{"declared extension", map[string]string{"package.json": `{"name":"a","pi":{"extensions":["./e.ts"]}}`}, true, false},
		{"conventional extensions dir", map[string]string{"package.json": `{"name":"a"}`, "extensions/e.ts": "x"}, true, false},
		{"bare dir loads as an extension", map[string]string{"package.json": `{"name":"a"}`}, true, false},
		{"bin", map[string]string{"package.json": `{"name":"a","bin":{"x":"x.js"},"pi":{"skills":["./s"]}}`}, true, false},
		{"postinstall", map[string]string{"package.json": `{"name":"a","scripts":{"postinstall":"x"},"pi":{"skills":["./s"]}}`}, true, false},
		{"missing manifest", map[string]string{"README.md": "x"}, false, true},
		{"malformed manifest", map[string]string{"package.json": `{"name":`}, false, true},
		{"resource key not an array", map[string]string{"package.json": `{"pi":{"skills":"./s"}}`}, false, true},
		{"entry escapes the package", map[string]string{"package.json": `{"pi":{"skills":["../../x"]}}`}, false, true},
		{"null resource is auto-discovery to Pi", map[string]string{"package.json": `{"pi":{"skills":null}}`}, false, true},
		{"pi not an object", map[string]string{"package.json": `{"pi":["x"]}`}, false, true},
		{"pi null", map[string]string{"package.json": `{"pi":null}`}, false, true},
		{"case-variant PI is not pi to Node", map[string]string{"package.json": `{"PI":{"skills":["./s"]}}`}, true, false},
		{"top level not an object", map[string]string{"package.json": `null`}, false, true},
	}
	for _, c := range cases {
		dir := t.TempDir()
		for f, body := range c.files {
			writeFile(t, filepath.Join(dir, f), body)
		}
		r, err := ReviewPackage(dir)
		if (err != nil) != c.fail || r.ShipsCode != c.code {
			t.Errorf("%s: code=%v err=%v, want code=%v fail=%v", c.name, r.ShipsCode, err, c.code, c.fail)
		}
	}
}

// fakeInstall replaces the installer: it records calls and env, and "installs" the package that build writes.
type fakeInstall struct {
	calls, deps int
	env         []string
	build       func(dir string, p Package)
	err         error
}

func (f *fakeInstall) use(t *testing.T) {
	oldFetch, oldDeps := fetchPackage, installDeps
	t.Cleanup(func() { fetchPackage, installDeps = oldFetch, oldDeps })
	fetchPackage = func(_ context.Context, _ io.Writer, p Package, slot string, env []string) error {
		f.calls++
		f.env = env
		if f.err != nil {
			return f.err
		}
		f.build(packageDir(slot, p), p)
		return nil
	}
	installDeps = func(context.Context, io.Writer, string, []string) error { f.deps++; return nil }
}

// codePkg writes an extension package of the pinned name and version.
func codePkg(t *testing.T) func(string, Package) {
	return func(dir string, p Package) {
		writeFile(t, filepath.Join(dir, "package.json"), `{"name":"`+p.Name+`","version":"`+p.Version+`","pi":{"extensions":["./e.ts"]}}`)
	}
}

func mustPkgs(t *testing.T, s string) []Package {
	t.Helper()
	ps, err := ParsePackages(s)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// testStore is a fresh state root and its package store.
func testStore(t *testing.T) (root, store string) {
	root = t.TempDir()
	return root, storeUnder(root)
}

func runSync(t *testing.T, root string, wanted, approved []Package) (string, []Loaded, error) {
	var out bytes.Buffer
	l, err := syncIn(context.Background(), &out, root, t.TempDir(), wanted, approved)
	return out.String(), l, err
}

func TestSyncUnapprovedCodeIsRemovedAndRefused(t *testing.T) {
	root, store := testStore(t)
	f := &fakeInstall{build: codePkg(t)}
	f.use(t)
	p := mustPkgs(t, "npm:evil@1.0.0")
	_, l, err := runSync(t, root, p, nil)
	if err == nil || l != nil || !strings.Contains(err.Error(), "claude-code configure agents pi packages_approved=npm:evil@1.0.0") ||
		!strings.Contains(err.Error(), "pi.extensions") {
		t.Fatalf("err = %v, loaded = %v", err, l)
	}
	if _, e := os.Stat(filepath.Join(store, slotName(p[0]))); e == nil {
		t.Error("unapproved package left installed")
	}
}

func TestSyncApprovedKeptThenNoOp(t *testing.T) {
	root, store := testStore(t)
	f := &fakeInstall{build: codePkg(t)}
	f.use(t)
	p := mustPkgs(t, "npm:good@1.0.0")
	out, l, err := runSync(t, root, p, p)
	if err != nil || len(l) != 1 || !strings.Contains(out, "installing npm:good@1.0.0") {
		t.Fatalf("first sync: %v %v %q", err, l, out)
	}
	if got := PackageArgv(l); !slices.Equal(got, []string{"-e", packageDir(filepath.Join(store, slotName(p[0])), p[0])}) {
		t.Errorf("argv = %v", got)
	}
	out, _, err = runSync(t, root, p, p)
	if err != nil || out != "" || f.calls != 1 {
		t.Errorf("second sync: err=%v out=%q installs=%d; want silent no-op", err, out, f.calls)
	}
}

func TestSyncRevokedApprovalStopsLoadingAndKeepsSlot(t *testing.T) {
	root, store := testStore(t)
	(&fakeInstall{build: codePkg(t)}).use(t)
	p := mustPkgs(t, "npm:good@1.0.0")
	if _, _, err := runSync(t, root, p, p); err != nil {
		t.Fatal(err)
	}
	if _, l, err := runSync(t, root, p, nil); err == nil || l != nil {
		t.Fatal("an installed package whose approval was removed still loads")
	}
	if _, e := os.Stat(filepath.Join(store, slotName(p[0]), specMarker)); e != nil {
		t.Error("a launch removed a complete slot a live run may load; removal is clean's")
	}
}

func TestSyncVersionMismatchRefused(t *testing.T) {
	root, store := testStore(t)
	f := &fakeInstall{build: func(dir string, p Package) {
		writeFile(t, filepath.Join(dir, "package.json"), `{"name":"`+p.Name+`","version":"1.0.1","pi":{"skills":["./s"]}}`)
	}}
	f.use(t)
	p := mustPkgs(t, "npm:skilly@1.0.0")
	if _, _, err := runSync(t, root, p, nil); err == nil || !strings.Contains(err.Error(), "does not match the pin") {
		t.Fatalf("err = %v", err)
	}
	if _, e := os.Stat(filepath.Join(store, slotName(p[0]))); e == nil {
		t.Error("mismatched package left installed")
	}
}

func TestSyncGitCommitMismatchRefused(t *testing.T) {
	root, _ := testStore(t)
	(&fakeInstall{build: func(dir string, _ Package) {
		writeFile(t, filepath.Join(dir, "package.json"), `{"name":"g","pi":{"skills":["./s"]}}`)
		writeFile(t, filepath.Join(dir, ".git", "HEAD"), strings.Repeat("b", 40)+"\n")
	}}).use(t)
	p := mustPkgs(t, "git:github.com/o/r@"+strings.Repeat("a", 40))
	if _, _, err := runSync(t, root, p, nil); err == nil || !strings.Contains(err.Error(), "does not match the pin") {
		t.Fatalf("err = %v", err)
	}
}

func TestSyncNeverPrunesAndEmptyListTouchesNothing(t *testing.T) {
	root, store := testStore(t)
	(&fakeInstall{build: codePkg(t)}).use(t)
	p := mustPkgs(t, "npm:good@1.0.0")
	if _, _, err := runSync(t, root, p, p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runSync(t, root, mustPkgs(t, "npm:other@1.0.0"), mustPkgs(t, "npm:other@1.0.0")); err != nil {
		t.Fatal(err)
	}
	if _, e := os.Stat(filepath.Join(store, slotName(p[0]))); e != nil {
		t.Error("a launch with another list pruned a slot a live run may load")
	}
	blocked := t.TempDir()
	writeFile(t, storeUnder(blocked), "not a dir") // an unusable store must not block a launch with nothing pinned
	if out, l, err := runSync(t, blocked, nil, nil); err != nil || l != nil || out != "" {
		t.Fatalf("empty list: err=%v loaded=%v out=%q", err, l, out)
	}
}

func TestSyncRefusesSymlinkedStore(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	victim := filepath.Join(outside, "npm-0123456789abcdef")
	writeFile(t, filepath.Join(victim, "keep"), "x")
	if err := os.MkdirAll(filepath.Join(root, "pi"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, storeUnder(root)); err != nil {
		t.Fatal(err)
	}
	f := &fakeInstall{build: codePkg(t)}
	f.use(t)
	if _, _, err := runSync(t, root, mustPkgs(t, "npm:good@1.0.0"), nil); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("err = %v, want a refused store", err)
	}
	if _, e := os.Stat(filepath.Join(victim, "keep")); e != nil || f.calls != 0 {
		t.Errorf("a symlinked store was used: installs=%d", f.calls)
	}
}

func TestGitDepsOnlyAfterReviewAndApproval(t *testing.T) {
	root, store := testStore(t)
	commit := strings.Repeat("a", 40)
	f := &fakeInstall{build: func(dir string, _ Package) {
		writeFile(t, filepath.Join(dir, "package.json"), `{"name":"g","dependencies":{"x":"1.0.0"},"pi":{"extensions":["./e.ts"]}}`)
		writeFile(t, filepath.Join(dir, ".git", "HEAD"), commit+"\n")
	}}
	f.use(t)
	p := mustPkgs(t, "git:github.com/o/r@"+commit)
	if _, _, err := runSync(t, root, p, nil); err == nil || f.deps != 0 {
		t.Fatalf("unapproved: err=%v deps installs=%d, want refused before any dependency install", err, f.deps)
	}
	if _, e := os.Stat(filepath.Join(store, slotName(p[0]))); e == nil {
		t.Error("an unapproved fresh git slot was kept")
	}
	if _, _, err := runSync(t, root, p, p); err != nil || f.deps != 1 {
		t.Fatalf("approved: err=%v deps installs=%d", err, f.deps)
	}
}

func TestInstallerArgvAndEnvAreHardened(t *testing.T) {
	git := strings.Join(gitArgv("fetch"), " ")
	for _, want := range []string{"core.hooksPath=" + os.DevNull, "core.fsmonitor=false", "protocol.file.allow=never", "core.symlinks=false"} {
		if !strings.Contains(git, want) {
			t.Errorf("git argv lacks %s: %s", want, git)
		}
	}
	npm := strings.Join(npmInstallArgs([]string{"a@1.0.0"}, "/p"), " ")
	for _, want := range []string{"--ignore-scripts", "--userconfig=" + os.DevNull, "--globalconfig=" + os.DevNull} {
		if !strings.Contains(npm, want) {
			t.Errorf("npm argv lacks %s: %s", want, npm)
		}
	}
	t.Setenv("npm_config_registry", "https://evil.example")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.hooksPath'='x'")
	env := strings.Join(installEnv(context.Background()), "\n")
	if strings.Contains(env, "evil.example") || strings.Contains(env, "core.hooksPath") ||
		!strings.Contains(env, "GIT_CONFIG_GLOBAL="+os.DevNull) || !strings.Contains(env, "GIT_CONFIG_NOSYSTEM=1") {
		t.Errorf("installer env not hardened:\n%s", env)
	}
}

func TestGitDepsRefuseARepoNpmrc(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".npmrc"), "registry=https://evil.example")
	if err := runDeps(context.Background(), io.Discard, dir, nil); err == nil || !strings.Contains(err.Error(), ".npmrc") {
		t.Errorf("a repo .npmrc did not refuse the dependency install: %v", err)
	}
}

func TestReviewRefusesEscapingSymlink(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(dir, "package.json"), `{"name":"a","pi":{"skills":["./skills"]}}`)
	writeFile(t, filepath.Join(dir, "skills", "s", "SKILL.md"), "x")
	writeFile(t, filepath.Join(outside, "secret"), "x")
	if err := os.Symlink(filepath.Join(dir, "skills", "s", "SKILL.md"), filepath.Join(dir, "inside")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReviewPackage(dir); err != nil {
		t.Fatalf("a link inside the package was refused: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "skills", "s", "out")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReviewPackage(dir); err == nil || !strings.Contains(err.Error(), "outside the package") {
		t.Fatalf("err = %v, want an escaping link refused", err)
	}
}

func TestSyncPartialSlotIsReinstalledAndFailedInstallLeavesNothing(t *testing.T) {
	root, store := testStore(t)
	f := &fakeInstall{build: codePkg(t)}
	f.use(t)
	p := mustPkgs(t, "npm:good@1.0.0")
	writeFile(t, filepath.Join(store, slotName(p[0]), "junk"), "x") // no .spec marker: a crashed install
	if _, _, err := runSync(t, root, p, p); err != nil || f.calls != 1 {
		t.Fatalf("err=%v installs=%d", err, f.calls)
	}
	if _, e := os.Stat(filepath.Join(store, slotName(p[0]), "junk")); e == nil {
		t.Error("partial slot content survived")
	}
	f2 := &fakeInstall{err: io.ErrUnexpectedEOF}
	f2.use(t)
	q := mustPkgs(t, "npm:other@1.0.0")
	if _, _, err := runSync(t, root, q, q); err == nil {
		t.Fatal("failed install did not fail the sync")
	}
	if _, e := os.Stat(filepath.Join(store, slotName(q[0]))); e == nil {
		t.Error("failed install left a slot")
	}
}

func TestSyncLocalPackages(t *testing.T) {
	store, cfg := t.TempDir(), t.TempDir() // store is the state root here
	writeFile(t, filepath.Join(cfg, "ok", "package.json"), `{"name":"ok","pi":{"skills":["./s"]}}`)
	writeFile(t, filepath.Join(cfg, "code", "package.json"), `{"name":"code","pi":{"extensions":["./e.ts"]}}`)
	var out bytes.Buffer
	ok := mustPkgs(t, "./ok")
	l, err := syncIn(context.Background(), &out, store, cfg, ok, nil)
	if err != nil || len(l) != 1 || l[0].Dir != filepath.Join(cfg, "ok") {
		t.Fatalf("skills-only local: %v %v", err, l)
	}
	code := mustPkgs(t, "./code")
	if _, err = syncIn(context.Background(), &out, store, cfg, code, nil); err == nil || !strings.Contains(err.Error(), "packages_approved=./code") {
		t.Fatalf("unapproved local code: %v", err)
	}
	if _, err = syncIn(context.Background(), &out, store, cfg, code, code); err != nil {
		t.Fatalf("approved local code: %v", err)
	}
	if _, e := os.Stat(filepath.Join(cfg, "code", "package.json")); e != nil {
		t.Error("a local package was deleted")
	}
}

func TestInstallEnvForbidsScriptsAndDropsCredentials(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "DATABRICKS_TOKEN", "OPENAI_API_KEY"} {
		t.Setenv(k, "s3cret")
	}
	root, _ := testStore(t)
	f := &fakeInstall{build: func(dir string, p Package) {
		writeFile(t, filepath.Join(dir, "package.json"), `{"name":"`+p.Name+`","version":"`+p.Version+`","pi":{"skills":["./s"]}}`)
	}}
	f.use(t)
	if _, _, err := runSync(t, root, mustPkgs(t, "npm:a@1.0.0"), nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.env, "npm_config_ignore_scripts=true") {
		t.Error("installer env lacks npm_config_ignore_scripts=true")
	}
	for _, e := range f.env {
		if strings.Contains(e, "s3cret") {
			t.Errorf("credential in installer env: %s", strings.SplitN(e, "=", 2)[0])
		}
	}
	if args := npmInstallArgs([]string{"a@1.0.0"}, "/p"); !slices.Contains(args, "--ignore-scripts") {
		t.Errorf("npm args lack --ignore-scripts: %v", args)
	}
}

func TestRemoveSlotRefusesOutsideTheStore(t *testing.T) {
	store := t.TempDir()
	outside := t.TempDir()
	good := filepath.Join(store, "npm-0123456789abcdef")
	if err := os.Mkdir(good, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, slot := range []string{outside, store, filepath.Join(store, "npm-0123456789abcdef", "x"), filepath.Join(store, "other"),
		filepath.Join(store, "..", "npm-0123456789abcdef"), filepath.Join(outside, "npm-0123456789abcdef")} {
		if err := removeSlot(store, slot); err == nil {
			t.Errorf("removeSlot(%q) accepted", slot)
		}
	}
	for _, root := range []string{"/", "relative"} {
		if err := removeSlot(root, filepath.Join(root, "npm-0123456789abcdef")); err == nil {
			t.Errorf("store %q accepted", root)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && removeSlot(home, filepath.Join(home, "npm-0123456789abcdef")) == nil {
		t.Error("the home directory was accepted as a store")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("outside dir was removed")
	}
	if err := removeSlot(store, good); err != nil {
		t.Errorf("a real slot refused: %v", err)
	}
}

func TestKeepSlotPrunesOnlyUnlistedRealSlots(t *testing.T) {
	root, store := testStore(t)
	listed, unlisted := mustPkgs(t, "npm:a@1.0.0")[0], mustPkgs(t, "npm:b@1.0.0")[0]
	for _, p := range []Package{listed, unlisted} {
		writeFile(t, filepath.Join(store, slotName(p), specMarker), p.Spec)
	}
	if !keepSlotIn(root, filepath.Join(store, slotName(listed)), []Package{listed}) {
		t.Error("a listed slot would be pruned")
	}
	if keepSlotIn(root, filepath.Join(store, slotName(unlisted)), []Package{listed}) {
		t.Error("an unlisted slot is kept")
	}
	if !keepSlotIn(root, filepath.Join(store, "sync.lock"), nil) {
		t.Error("a non-slot entry would be pruned")
	}
	linked, outside := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(outside, slotName(unlisted), "keep"), "x")
	if err := os.MkdirAll(filepath.Join(linked, "pi"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, storeUnder(linked)); err != nil {
		t.Fatal(err)
	}
	if !keepSlotIn(linked, filepath.Join(storeUnder(linked), slotName(unlisted)), nil) {
		t.Error("a slot reached through a symlinked store would be pruned")
	}
}
