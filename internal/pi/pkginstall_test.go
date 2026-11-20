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
	calls int
	env   []string
	build func(dir string, p Package)
	err   error
}

func (f *fakeInstall) use(t *testing.T) {
	old := installPackage
	t.Cleanup(func() { installPackage = old })
	installPackage = func(_ context.Context, _ io.Writer, p Package, slot string, env []string) error {
		f.calls++
		f.env = env
		if f.err != nil {
			return f.err
		}
		f.build(packageDir(slot, p), p)
		return nil
	}
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

func runSync(t *testing.T, store string, wanted, approved []Package) (string, []Loaded, error) {
	var out bytes.Buffer
	l, err := syncIn(context.Background(), &out, store, t.TempDir(), wanted, approved)
	return out.String(), l, err
}

func TestSyncUnapprovedCodeIsRemovedAndRefused(t *testing.T) {
	store := t.TempDir()
	f := &fakeInstall{build: codePkg(t)}
	f.use(t)
	p := mustPkgs(t, "npm:evil@1.0.0")
	_, l, err := runSync(t, store, p, nil)
	if err == nil || l != nil || !strings.Contains(err.Error(), "claude-code configure agents pi packages_approved=npm:evil@1.0.0") ||
		!strings.Contains(err.Error(), "pi.extensions") {
		t.Fatalf("err = %v, loaded = %v", err, l)
	}
	if _, e := os.Stat(filepath.Join(store, slotName(p[0]))); e == nil {
		t.Error("unapproved package left installed")
	}
}

func TestSyncApprovedKeptThenNoOp(t *testing.T) {
	store := t.TempDir()
	f := &fakeInstall{build: codePkg(t)}
	f.use(t)
	p := mustPkgs(t, "npm:good@1.0.0")
	out, l, err := runSync(t, store, p, p)
	if err != nil || len(l) != 1 || !strings.Contains(out, "installing npm:good@1.0.0") {
		t.Fatalf("first sync: %v %v %q", err, l, out)
	}
	if got := PackageArgv(l); !slices.Equal(got, []string{"-e", packageDir(filepath.Join(store, slotName(p[0])), p[0])}) {
		t.Errorf("argv = %v", got)
	}
	out, _, err = runSync(t, store, p, p)
	if err != nil || out != "" || f.calls != 1 {
		t.Errorf("second sync: err=%v out=%q installs=%d; want silent no-op", err, out, f.calls)
	}
}

func TestSyncRevokedApprovalRemovesInstalledPackage(t *testing.T) {
	store := t.TempDir()
	(&fakeInstall{build: codePkg(t)}).use(t)
	p := mustPkgs(t, "npm:good@1.0.0")
	if _, _, err := runSync(t, store, p, p); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runSync(t, store, p, nil); err == nil {
		t.Fatal("an installed package whose approval was removed still loads")
	}
	if _, e := os.Stat(filepath.Join(store, slotName(p[0]))); e == nil {
		t.Error("slot kept after approval was revoked")
	}
}

func TestSyncVersionMismatchRefused(t *testing.T) {
	store := t.TempDir()
	f := &fakeInstall{build: func(dir string, p Package) {
		writeFile(t, filepath.Join(dir, "package.json"), `{"name":"`+p.Name+`","version":"1.0.1","pi":{"skills":["./s"]}}`)
	}}
	f.use(t)
	p := mustPkgs(t, "npm:skilly@1.0.0")
	if _, _, err := runSync(t, store, p, nil); err == nil || !strings.Contains(err.Error(), "does not match the pin") {
		t.Fatalf("err = %v", err)
	}
	if _, e := os.Stat(filepath.Join(store, slotName(p[0]))); e == nil {
		t.Error("mismatched package left installed")
	}
}

func TestSyncGitCommitMismatchRefused(t *testing.T) {
	store := t.TempDir()
	(&fakeInstall{build: func(dir string, _ Package) {
		writeFile(t, filepath.Join(dir, "package.json"), `{"name":"g","pi":{"skills":["./s"]}}`)
		writeFile(t, filepath.Join(dir, ".git", "HEAD"), strings.Repeat("b", 40)+"\n")
	}}).use(t)
	p := mustPkgs(t, "git:github.com/o/r@"+strings.Repeat("a", 40))
	if _, _, err := runSync(t, store, p, nil); err == nil || !strings.Contains(err.Error(), "does not match the pin") {
		t.Fatalf("err = %v", err)
	}
}

func TestSyncUnlistedRemoved(t *testing.T) {
	store := t.TempDir()
	(&fakeInstall{build: codePkg(t)}).use(t)
	p := mustPkgs(t, "npm:good@1.0.0")
	if _, _, err := runSync(t, store, p, p); err != nil {
		t.Fatal(err)
	}
	out, l, err := runSync(t, store, nil, nil)
	if err != nil || len(l) != 0 || !strings.Contains(out, "removed npm:good@1.0.0") {
		t.Fatalf("err=%v loaded=%v out=%q", err, l, out)
	}
	if _, e := os.Stat(filepath.Join(store, slotName(p[0]))); e == nil {
		t.Error("unlisted package still installed")
	}
}

func TestSyncPartialSlotIsReinstalledAndFailedInstallLeavesNothing(t *testing.T) {
	store := t.TempDir()
	f := &fakeInstall{build: codePkg(t)}
	f.use(t)
	p := mustPkgs(t, "npm:good@1.0.0")
	writeFile(t, filepath.Join(store, slotName(p[0]), "junk"), "x") // no .spec marker: a crashed install
	if _, _, err := runSync(t, store, p, p); err != nil || f.calls != 1 {
		t.Fatalf("err=%v installs=%d", err, f.calls)
	}
	if _, e := os.Stat(filepath.Join(store, slotName(p[0]), "junk")); e == nil {
		t.Error("partial slot content survived")
	}
	f2 := &fakeInstall{err: io.ErrUnexpectedEOF}
	f2.use(t)
	q := mustPkgs(t, "npm:other@1.0.0")
	if _, _, err := runSync(t, store, q, q); err == nil {
		t.Fatal("failed install did not fail the sync")
	}
	if _, e := os.Stat(filepath.Join(store, slotName(q[0]))); e == nil {
		t.Error("failed install left a slot")
	}
}

func TestSyncLocalPackages(t *testing.T) {
	store, cfg := t.TempDir(), t.TempDir()
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
	store := t.TempDir()
	f := &fakeInstall{build: func(dir string, p Package) {
		writeFile(t, filepath.Join(dir, "package.json"), `{"name":"`+p.Name+`","version":"`+p.Version+`","pi":{"skills":["./s"]}}`)
	}}
	f.use(t)
	if _, _, err := runSync(t, store, mustPkgs(t, "npm:a@1.0.0"), nil); err != nil {
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
