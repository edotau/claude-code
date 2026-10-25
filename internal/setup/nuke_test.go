package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func fixtureNukeDir(t *testing.T) string {
	t.Helper()
	dir, _ := vaulttest.UseKeyring(t)
	t.Setenv("CLAUDE_CONFIG_DIR", dir) // the keep-memory bank lock lives under ConfigDir: keep it off the real ~/.claude
	gitInit(t, dir, map[string]string{
		"CLAUDE.md":   "harness\n",
		"agents/a.md": "agent\n",
	})
	mustWrite(t, filepath.Join(dir, "settings.local.json"), `{"local":true}`)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{}}`)
	vaulttest.Put(t, vault.Pins, "HARNESS_PROVIDER", "foo")
	vaulttest.Secret(t, "Z_API_KEY", "other-test-value")
	vaulttest.Secret(t, "API_KEY", "sk-super-secret")
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
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = data
	}
	return out
}

func nukeVaultMeta(t *testing.T, path string) (id, kid string) {
	t.Helper()
	if err := vault.ViewAt(path, func(tx *vault.Tx) error {
		id, _ = tx.Get(vault.Meta, "vault_id")
		kid, _ = tx.Get(vault.Meta, "kid")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if id == "" || kid == "" {
		t.Fatal("seeded vault must have vault_id and kid")
	}
	return id, kid
}

func TestNukePlanListsClassesAndSecretNamesOnly(t *testing.T) {
	dir := fixtureNukeDir(t)
	id, kid := nukeVaultMeta(t, vault.Path())
	_, keyring := vaulttest.UseKeyring(t)
	keyring.Down = true
	t.Setenv("HARNESS_VAULT_KEY", "invalid-test-key")
	plan, err := Plan(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.HasSecrets {
		t.Fatal("expected secrets present")
	}
	if !slices.Equal(plan.SecretNames, []string{"API_KEY", "Z_API_KEY"}) {
		t.Fatalf("secret names = %v", plan.SecretNames)
	}
	if plan.VaultID != id || plan.Kid != kid {
		t.Fatalf("plan vault identity = %q/%q, want %q/%q", plan.VaultID, plan.Kid, id, kid)
	}
	if !slices.Contains(plan.UserContent, "state/vault.db") {
		t.Fatalf("plan must archive the vault: %v", plan.UserContent)
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
	if strings.Contains(out.String(), "sk-super-secret") || strings.Contains(out.String(), "other-test-value") {
		t.Fatal("plan output must never contain the secret value")
	}
	if !strings.Contains(out.String(), "API_KEY") || !strings.Contains(out.String(), "Z_API_KEY") {
		t.Fatal("plan output should list the secret name")
	}
}

func TestNukePlanWithoutSecrets(t *testing.T) {
	for _, withVault := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing vault", true: "pins only"}[withVault], func(t *testing.T) {
			dir := vaulttest.Use(t)
			if withVault {
				vaulttest.Put(t, vault.Pins, "HARNESS_PROVIDER", "foo")
			}
			plan, err := Plan(dir, false)
			if err != nil {
				t.Fatal(err)
			}
			if plan.HasSecrets || len(plan.SecretNames) != 0 {
				t.Fatalf("plan reports unseeded secrets: %+v", plan)
			}
			if slices.Contains(plan.UserContent, "state/vault.db") != withVault {
				t.Fatalf("vault archive presence does not match fixture: %v", plan.UserContent)
			}
		})
	}
}

// TestNukePlanWarnsOnUnreadableVaultInsteadOfFailing: an unreadable/corrupt vault must not block the
// nuke — Plan now reports it as VaultWarning (still archived opaquely by Nuke) rather than failing outright.
func TestNukePlanWarnsOnUnreadableVaultInsteadOfFailing(t *testing.T) {
	dir := vaulttest.Use(t)
	vaulttest.Put(t, vault.Meta, "schema", "999")
	plan, err := Plan(dir, false)
	if err != nil {
		t.Fatalf("Plan = %v, want nil error (an unreadable vault becomes a warning, not a failure)", err)
	}
	if !strings.Contains(plan.VaultWarning, "schema 999") {
		t.Errorf("VaultWarning = %q, want it to mention schema 999", plan.VaultWarning)
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

// TestCreateBackupFileMode0600: the nuke backup tarball must be created 0600, never world/group
// readable — it holds the sealed vault and archived settings.
func TestCreateBackupFileMode0600(t *testing.T) {
	home := t.TempDir()
	dest, f, err := createBackupFile(home)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("backup file mode = %o, want 0600", got)
	}
}

// TestNukeApplyProceedsWhenVaultUnreadable: an unreadable vault (bad schema) only warns in the plan;
// Apply still archives (the vault.db bytes raw) and removes dir rather than refusing outright.
func TestNukeApplyProceedsWhenVaultUnreadable(t *testing.T) {
	dir := fixtureNukeDir(t)
	vaulttest.Put(t, vault.Meta, "schema", "999")
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := Nuke(NukeOpts{Dir: dir, Apply: true, Confirm: dir}); err != nil {
		t.Fatalf("Nuke = %v, want it to proceed past an unreadable vault", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir should be removed, err=%v", err)
	}
}

func TestNukeApplyArchivesVaultNeverPlaintextSecrets(t *testing.T) {
	dir := fixtureNukeDir(t)
	id, kid := nukeVaultMeta(t, vault.Path())
	vaulttest.Keyring(t).Down = true
	t.Setenv("HARNESS_VAULT_KEY", "invalid-test-key")
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := Nuke(NukeOpts{Dir: dir, Apply: true, Confirm: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir should be removed, err=%v", err)
	}

	entries := tarEntries(t, findTarball(t, home))
	for _, want := range []string{"manifest.json", "settings.local.json", "state/vault.db"} {
		if _, ok := entries[want]; !ok {
			t.Fatalf("tarball missing %q; has %v", want, tarNames(entries))
		}
	}
	for name, data := range entries {
		if strings.Contains(name, "secrets.env") {
			t.Fatalf("tarball must never contain secrets.env, got %q", name)
		}
		if bytes.Contains(data, []byte("sk-super-secret")) || bytes.Contains(data, []byte("other-test-value")) {
			t.Fatalf("tarball entry %q must not contain plaintext secret values", name)
		}
	}
	manifest := string(entries["manifest.json"])
	if strings.Contains(manifest, "sk-super-secret") || strings.Contains(manifest, "other-test-value") {
		t.Fatal("manifest must never contain the secret value")
	}
	if !strings.Contains(manifest, "API_KEY") {
		t.Fatal("manifest should name the secret key")
	}
	var metadata struct {
		SecretNames []string `json:"secretNames"`
		VaultID     string   `json:"vaultId"`
		Kid         string   `json:"kid"`
	}
	if err := json.Unmarshal(entries["manifest.json"], &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.VaultID != id || metadata.Kid != kid {
		t.Fatalf("manifest vault identity = %q/%q, want %q/%q", metadata.VaultID, metadata.Kid, id, kid)
	}
	if !slices.Equal(metadata.SecretNames, []string{"API_KEY", "Z_API_KEY"}) {
		t.Fatalf("manifest secret names = %v", metadata.SecretNames)
	}
	restored := filepath.Join(t.TempDir(), "state", "vault.db")
	mustWrite(t, restored, string(entries["state/vault.db"]))
	if err := vault.ViewAt(restored, func(tx *vault.Tx) error {
		if names := tx.Names(vault.Secrets); !slices.Equal(names, metadata.SecretNames) {
			t.Errorf("archived vault secret names = %v, want %v", names, metadata.SecretNames)
		}
		if pin, _ := tx.Get(vault.Pins, "HARNESS_PROVIDER"); pin != "foo" {
			t.Errorf("archived provider pin = %q, want foo", pin)
		}
		if overlay, _ := tx.Get(vault.Overlay, "providers"); overlay != `{"providers":{}}` {
			t.Errorf("archived provider overlay = %q", overlay)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if restoredID, restoredKid := nukeVaultMeta(t, restored); restoredID != id || restoredKid != kid {
		t.Fatalf("archived vault identity = %q/%q, want %q/%q", restoredID, restoredKid, id, kid)
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

// --keep-memory also archives plans/ and a per-bank knowledge/ when present, not just memory/ and the bank.
func TestNukeKeepMemoryIncludesPlansAndKnowledge(t *testing.T) {
	dir := fixtureNukeDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	mustWrite(t, filepath.Join(dir, "plans", "2026-10-18-x.md"), "plan\n")
	mustWrite(t, filepath.Join(dir, "memory", "repo", "knowledge", "x.md"), "knowledge\n")

	if err := Nuke(NukeOpts{Dir: dir, Apply: true, Confirm: dir, KeepMemory: true}); err != nil {
		t.Fatal(err)
	}
	entries := tarEntries(t, findTarball(t, home))
	for _, want := range []string{"plans/2026-10-18-x.md", "memory/repo/knowledge/x.md"} {
		if _, ok := entries[want]; !ok {
			t.Fatalf("--keep-memory should archive %q, got %v", want, tarNames(entries))
		}
	}
}

// The dry-run preview lists the keep-memory set, so the user reviews it before the irreversible apply.
func TestNukePreviewShowsKeepMemorySet(t *testing.T) {
	dir := fixtureNukeDir(t)
	mustWrite(t, filepath.Join(dir, "memory", ".git", "HEAD"), "ref: refs/heads/main\n")
	mustWrite(t, filepath.Join(dir, "plans", "2026-10-18-x.md"), "plan\n")
	var out bytes.Buffer
	if err := Nuke(NukeOpts{Dir: dir, KeepMemory: true, Out: &out}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"memory/.git/HEAD", "plans/2026-10-18-x.md", "docs/memory/bank/repo/progress.md"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("preview with --keep-memory should list %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "memory", ".git", "HEAD")); err != nil {
		t.Fatal("preview must not remove anything")
	}
	out.Reset()
	if err := Nuke(NukeOpts{Dir: dir, KeepMemory: false, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "memory/.git/HEAD") {
		t.Error("preview without --keep-memory must not list memory/")
	}
}

// Without --keep-memory, memory/ (incl. its per-bank knowledge/) and plans/ are never archived (plan-only paths).
func TestNukeWithoutKeepMemorySkipsMemoryPlansKnowledge(t *testing.T) {
	dir := fixtureNukeDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	mustWrite(t, filepath.Join(dir, "memory", "repo", "MEMORY.md"), "- topic\n")
	mustWrite(t, filepath.Join(dir, "plans", "2026-10-18-x.md"), "plan\n")
	mustWrite(t, filepath.Join(dir, "memory", "repo", "knowledge", "x.md"), "knowledge\n")

	if err := Nuke(NukeOpts{Dir: dir, Apply: true, Confirm: dir, KeepMemory: false}); err != nil {
		t.Fatal(err)
	}
	entries := tarEntries(t, findTarball(t, home))
	for name := range entries {
		if strings.HasPrefix(name, "memory/") || strings.HasPrefix(name, "plans/") {
			t.Fatalf("nuke without --keep-memory must never archive %q", name)
		}
	}
}

// A symlinked projects/*/memory dir anywhere above a real one is still skipped: Lstat gates the
// candidate root itself before WalkDir ever runs, so a symlinked root contributes nothing.
func TestNukeKeepMemorySkipsSymlinkedCandidateRoot(t *testing.T) {
	dir := fixtureNukeDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	realStore := filepath.Join(t.TempDir(), "elsewhere")
	mustWrite(t, filepath.Join(realStore, "MEMORY.md"), "outside\n")
	if err := os.MkdirAll(filepath.Join(dir, "projects", "-linked-only"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realStore, filepath.Join(dir, "projects", "-linked-only", "memory")); err != nil {
		t.Fatal(err)
	}

	if err := Nuke(NukeOpts{Dir: dir, Apply: true, Confirm: dir, KeepMemory: true}); err != nil {
		t.Fatal(err)
	}
	entries := tarEntries(t, findTarball(t, home))
	for name := range entries {
		if strings.Contains(name, "linked-only") {
			t.Fatalf("symlinked candidate root must be skipped, got %q", name)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(realStore, "MEMORY.md")); string(b) != "outside\n" {
		t.Error("the symlink target outside the config dir must never be touched")
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

// --keep-memory archives the local memory Git store whole (incl. .git) and real native dirs, but a
// symlinked projects/*/memory (the store's own duplicate) and symlinked files are skipped, never followed.
func TestNukeKeepMemoryArchivesStoreSkipsSymlinks(t *testing.T) {
	dir := fixtureNukeDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	mustWrite(t, filepath.Join(dir, "memory", ".git", "HEAD"), "ref: refs/heads/main\n")
	mustWrite(t, filepath.Join(dir, "memory", "repo", "MEMORY.md"), "- topic\n")
	mustWrite(t, filepath.Join(dir, "projects", "-real", "memory", "MEMORY.md"), "native\n")
	escape := filepath.Join(t.TempDir(), "secret.txt")
	mustWrite(t, escape, "outside\n")
	if err := os.MkdirAll(filepath.Join(dir, "projects", "-linked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "memory", "repo"), filepath.Join(dir, "projects", "-linked", "memory")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escape, filepath.Join(dir, "memory", "repo", "escape.md")); err != nil {
		t.Fatal(err)
	}
	got, err := keepMemoryPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"memory/.git/HEAD", "memory/repo/MEMORY.md", "projects/-real/memory/MEMORY.md", "docs/memory/bank/repo/progress.md"} {
		if !slices.Contains(got, want) {
			t.Errorf("keepMemoryPaths missing %s: %v", want, got)
		}
	}
	for _, p := range got {
		if strings.HasPrefix(p, "projects/-linked/") || strings.HasSuffix(p, "escape.md") {
			t.Errorf("keepMemoryPaths followed a symlink: %s", p)
		}
	}
	if err := Nuke(NukeOpts{Dir: dir, Apply: true, Confirm: dir, KeepMemory: true}); err != nil {
		t.Fatal(err)
	}
	entries := tarEntries(t, findTarball(t, home))
	if string(entries["memory/.git/HEAD"]) != "ref: refs/heads/main\n" {
		t.Errorf("tarball lacks the memory Git dir: %v", tarNames(entries))
	}
	if b, _ := os.ReadFile(escape); string(b) != "outside\n" {
		t.Error("symlink target outside the config dir was touched")
	}
}
