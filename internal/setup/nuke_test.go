package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureNukeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitInit(t, dir, map[string]string{
		"CLAUDE.md":   "harness\n",
		"agents/a.md": "agent\n",
	})
	mustWrite(t, filepath.Join(dir, "settings.local.json"), `{"local":true}`)
	mustWrite(t, filepath.Join(dir, "providers.json"), `{"providers":{}}`)
	mustWrite(t, filepath.Join(dir, "env.d", "provider.env"), "HARNESS_PROVIDER=foo\n")
	mustWrite(t, filepath.Join(dir, "env.d", "secrets.env"), "API_KEY=sk-super-secret\n")
	mustWrite(t, filepath.Join(dir, "skills", "mine", "SKILL.md"), "mine\n")
	mustWrite(t, filepath.Join(dir, "docs", "memory", "bank", "repo", "progress.md"), "bank\n")
	return dir
}

func findTarball(t *testing.T, home string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(home, "claude-nuke-*.tgz"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one tarball in %s, got %v (err=%v)", home, matches, err)
	}
	return matches[0]
}

func tarEntries(t *testing.T, path string) map[string][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		out[hdr.Name] = data
	}
	return out
}

func TestNukePlanListsClassesAndSecretNamesOnly(t *testing.T) {
	dir := fixtureNukeDir(t)
	plan, err := Plan(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.HasSecrets {
		t.Fatal("expected secrets present")
	}
	if len(plan.SecretNames) != 1 || plan.SecretNames[0] != "API_KEY" {
		t.Fatalf("secret names = %v", plan.SecretNames)
	}
	byName := map[string]Entry{}
	for _, e := range plan.Entries {
		byName[e.Name] = e
	}
	if byName["CLAUDE.md"].Class != "tracked" {
		t.Fatalf("CLAUDE.md class = %q, want tracked", byName["CLAUDE.md"].Class)
	}
	if byName["skills"].Class != "untracked" {
		t.Fatalf("skills class = %q, want untracked", byName["skills"].Class)
	}

	var out bytes.Buffer
	printPlan(&out, plan)
	if strings.Contains(out.String(), "sk-super-secret") {
		t.Fatal("plan output must never contain the secret value")
	}
	if !strings.Contains(out.String(), "API_KEY") {
		t.Fatal("plan output should list the secret name")
	}
}

func TestNukeApplyRefusedOnWrongConfirm(t *testing.T) {
	dir := fixtureNukeDir(t)

	if err := Nuke(NukeOpts{Dir: dir, Apply: true, Confirm: "not-the-dir"}); err == nil {
		t.Fatal("want a refusal on a mismatched confirmation")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("dir must survive a refused nuke")
	}
}

func TestNukeApplyArchivesUserContentNeverSecrets(t *testing.T) {
	dir := fixtureNukeDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := Nuke(NukeOpts{Dir: dir, Apply: true, Confirm: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir should be removed, err=%v", err)
	}

	entries := tarEntries(t, findTarball(t, home))
	for _, want := range []string{"manifest.json", "settings.local.json", "providers.json", "env.d/provider.env"} {
		if _, ok := entries[want]; !ok {
			t.Fatalf("tarball missing %q; has %v", want, tarNames(entries))
		}
	}
	for name := range entries {
		if strings.Contains(name, "secrets.env") {
			t.Fatalf("tarball must never contain secrets.env, got %q", name)
		}
	}
	manifest := string(entries["manifest.json"])
	if strings.Contains(manifest, "sk-super-secret") {
		t.Fatal("manifest must never contain the secret value")
	}
	if !strings.Contains(manifest, "API_KEY") {
		t.Fatal("manifest should name the secret key")
	}
}

func TestNukeKeepMemoryAddsBank(t *testing.T) {
	dir := fixtureNukeDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := Nuke(NukeOpts{Dir: dir, Apply: true, Confirm: dir, KeepMemory: true}); err != nil {
		t.Fatal(err)
	}
	entries := tarEntries(t, findTarball(t, home))
	if _, ok := entries["docs/memory/bank/repo/progress.md"]; !ok {
		t.Fatalf("--keep-memory should add the bank, got %v", tarNames(entries))
	}
}

func tarNames(entries map[string][]byte) []string {
	out := make([]string, 0, len(entries))
	for k := range entries {
		out = append(out, k)
	}
	return out
}

// The floor holds before any plan or prompt: "/" and $HOME are never nuked, confirmed or not.
func TestNukeRefusesRootAndHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var out bytes.Buffer
	for _, dir := range []string{"/", home, home + "/"} {
		err := Nuke(NukeOpts{Dir: dir, Apply: true, Confirm: dir, Out: &out})
		if !errors.Is(err, ErrNotConfigDir) {
			t.Errorf("Nuke(%q) = %v, want ErrNotConfigDir", dir, err)
		}
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatal("home was removed")
	}
}
