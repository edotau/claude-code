package shadow

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/sqlite"
)

// readTree maps every file under dir (slash paths) to its bytes.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = string(b)
		return rerr
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return out
}

// bootstrapSource builds a db-truth source with two entries and returns its db path and exported Markdown.
func bootstrapSource(t *testing.T) (string, map[string]string) {
	t.Helper()
	e := newParityEnv(t)
	truthAdd(t, e, "first")
	e.step++
	truthAdd(t, e, "second")
	return ShadowDBPath(), readTree(t, paths.MemoryStoreDir())
}

// freshDest points the process at an empty config dir and returns the destination db path.
func freshDest(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := os.MkdirAll(filepath.Dir(ShadowDBPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	return ShadowDBPath()
}

func TestBootstrapRefusesFilesTruth(t *testing.T) {
	src, _ := bootstrapSource(t)
	dst := freshDest(t)
	pinFilesTruth(t)
	before := readTree(t, paths.ConfigDir())
	_, err := BootstrapCopy(context.Background(), src, dst)
	if err == nil || !strings.Contains(err.Error(), "truth") {
		t.Fatalf("files-truth destination: err = %v", err)
	}
	if after := readTree(t, paths.ConfigDir()); !maps.Equal(before, after) {
		t.Fatalf("refused bootstrap wrote: %v -> %v", before, after)
	}
}

func TestBootstrapRefusesBankFiles(t *testing.T) {
	src, _ := bootstrapSource(t)
	for _, name := range append(memory.BankFiles(), "MEMORY.md", "index.md", "catalog.md") {
		t.Run(name, func(t *testing.T) {
			dst := freshDest(t)
			envOK(t, os.MkdirAll(paths.MemoryStoreDir(), 0o755) == nil, "mkdir store")
			envOK(t, os.WriteFile(memory.Path(name), []byte("# mine\n"), 0o600) == nil, "write %s", name)
			before := readTree(t, paths.ConfigDir())
			_, err := BootstrapCopy(context.Background(), src, dst)
			envOK(t, err != nil && strings.Contains(err.Error(), "already holds"), "destination %s: %v", name, err)
			envOK(t, maps.Equal(before, readTree(t, paths.ConfigDir())), "refused bootstrap changed %s", name)
		})
	}
}

func TestBootstrapFreshDestinationIsDBTruth(t *testing.T) {
	src, want := bootstrapSource(t)
	dst := freshDest(t)
	if _, err := BootstrapCopy(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	tb, _ := os.ReadFile(filepath.Join(paths.StateDir(), "memory", "truth"))
	envOK(t, strings.TrimSpace(string(tb)) == "db", "truth file = %q, want db", tb)
	got := readTree(t, paths.MemoryStoreDir())
	for _, name := range memory.BankFiles() {
		envOK(t, want[name] != "" && got[name] == want[name], "%s differs: want %q, got %q", name, want[name], got[name])
	}
	_, v1, err := memory.NewBankStore(false).Load()
	if err != nil {
		t.Fatal(err)
	}
	entries := shadowCount(t, `SELECT count(*) FROM entries WHERE deleted = 0`)
	changes := shadowCount(t, `SELECT count(*) FROM changes`)
	_, v2, err := memory.NewBankStore(false).Load()
	if err != nil {
		t.Fatal(err)
	}
	envOK(t, maps.Equal(v1, v2) && len(v1) > 0, "second open moved versions: %v -> %v", v1, v2)
	n := shadowCount(t, `SELECT count(*) FROM entries WHERE deleted = 0`)
	envOK(t, n == entries && n >= 2, "live rows %d -> %d", entries, n)
	n = shadowCount(t, `SELECT count(*) FROM changes`)
	envOK(t, n == changes, "history rows %d -> %d: re-import", changes, n)
	envOK(t, maps.Equal(got, readTree(t, paths.MemoryStoreDir())), "second open rewrote Markdown")
}

func TestBootstrapDoesNotImportDestinationNotes(t *testing.T) {
	src, _ := bootstrapSource(t)
	dst := freshDest(t)
	note := filepath.Join(paths.MemoryStoreDir(), "notes", "destination.md")
	envOK(t, os.MkdirAll(filepath.Dir(note), 0o700) == nil, "mkdir notes")
	envOK(t, os.WriteFile(note, []byte("destination only\n"), 0o600) == nil, "write note")
	_, err := BootstrapCopy(context.Background(), src, dst)
	envOK(t, err == nil, "bootstrap: %v", err)
	db, err := sqlite.Open(dst, sqlite.Options{})
	envOK(t, err == nil, "open: %v", err)
	t.Cleanup(func() { _ = db.Close() })
	var n int
	err = db.QueryRow(`SELECT count(*) FROM docs WHERE path = 'notes/destination.md'`).Scan(&n)
	envOK(t, err == nil && n == 0, "destination note imported: %d (%v)", n, err)
	data, err := os.ReadFile(note)
	envOK(t, err == nil && string(data) == "destination only\n", "destination note changed: %q (%v)", data, err)
	for _, name := range memory.BankFiles() {
		data, err := os.ReadFile(memory.Path(name))
		envOK(t, err == nil, "read %s: %v", name, err)
		fi, err := os.Stat(memory.Path(name))
		envOK(t, err == nil, "stat %s: %v", name, err)
		var sum, stamp string
		err = db.QueryRow(`SELECT value FROM meta WHERE key = ?`, "render:"+name).Scan(&sum)
		envOK(t, err == nil && sum == fmt.Sprintf("%x", sha256.Sum256(data)), "render %s: %q (%v)", name, sum, err)
		err = db.QueryRow(`SELECT value FROM meta WHERE key = ?`, "stat:"+name).Scan(&stamp)
		envOK(t, err == nil && stamp == fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano()), "stat %s: %q (%v)", name, stamp, err)
	}
}

func TestBootstrapRejectsInactiveDestination(t *testing.T) {
	src, _ := bootstrapSource(t)
	freshDest(t)
	dst := filepath.Join(t.TempDir(), "elsewhere.db")
	_, err := BootstrapCopy(context.Background(), src, dst)
	envOK(t, err != nil, "bootstrap accepted a db other than the destination store")
	_, err = os.Lstat(dst)
	envOK(t, os.IsNotExist(err), "inactive destination written: %v", err)
	_, err = os.Lstat(filepath.Join(paths.StateDir(), "memory", "truth"))
	envOK(t, os.IsNotExist(err), "inactive bootstrap set truth: %v", err)
}

func TestBootstrapRefusesCopiedDocumentCollision(t *testing.T) {
	src, _ := bootstrapSource(t)
	db, err := sqlite.Open(src, sqlite.Options{})
	envOK(t, err == nil, "open: %v", err)
	_, err = db.Exec(`INSERT INTO docs(path, kind, header, grammar) VALUES('notes/shared.md', 'note', '', 'note')`)
	envOK(t, errors.Join(err, db.Close()) == nil, "source doc: %v", err)
	dst := freshDest(t)
	note := filepath.Join(paths.MemoryStoreDir(), "notes", "shared.md")
	envOK(t, os.MkdirAll(filepath.Dir(note), 0o700) == nil, "mkdir")
	envOK(t, os.WriteFile(note, []byte("mine\n"), 0o600) == nil, "write")
	id, err := BootstrapCopy(context.Background(), src, dst)
	envOK(t, err != nil && id == "", "collision published copy %q (%v)", id, err)
	_, err = os.Lstat(dst)
	envOK(t, os.IsNotExist(err), "collision published destination: %v", err)
	data, err := os.ReadFile(note)
	envOK(t, err == nil && string(data) == "mine\n", "collision changed note: %q (%v)", data, err)
}

func TestBootstrapRefusesOldSchema(t *testing.T) {
	src := filepath.Join(t.TempDir(), "old.db")
	db, err := sqlite.Open(src, sqlite.Options{})
	envOK(t, err == nil, "open: %v", err)
	_, err = sqlite.Migrate(context.Background(), db, memorySteps[:1], sqlite.MigrateOptions{})
	envOK(t, errors.Join(err, db.Close()) == nil, "migrate old source: %v", err)
	dst := freshDest(t)
	_, err = BootstrapCopy(context.Background(), src, dst)
	envOK(t, err != nil && strings.Contains(err.Error(), "schema"), "old schema accepted: %v", err)
	_, err = os.Lstat(dst)
	envOK(t, os.IsNotExist(err), "old schema published: %v", err)
}

func TestBootstrapCanceled(t *testing.T) {
	src, _ := bootstrapSource(t)
	dst := freshDest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := BootstrapCopy(ctx, src, dst)
	envOK(t, errors.Is(err, context.Canceled), "canceled bootstrap: %v", err)
	_, err = os.Lstat(dst)
	envOK(t, os.IsNotExist(err), "canceled bootstrap published: %v", err)
	_, err = os.Lstat(filepath.Join(paths.StateDir(), "memory", "truth"))
	envOK(t, os.IsNotExist(err), "canceled bootstrap set truth: %v", err)
	left, err := filepath.Glob(dst + ".tmp-*")
	envOK(t, err == nil && len(left) == 0, "canceled bootstrap left temps: %v (%v)", left, err)
}

func TestBootstrapRefusesDestinationSidecars(t *testing.T) {
	src, _ := bootstrapSource(t)
	for _, suffix := range []string{"-wal", "-shm"} {
		dst := freshDest(t)
		envOK(t, os.WriteFile(dst+suffix, []byte("old sidecar"), 0o600) == nil, "write sidecar")
		_, err := BootstrapCopy(context.Background(), src, dst)
		envOK(t, err != nil, "stale %s accepted", suffix)
		_, err = os.Lstat(dst)
		envOK(t, os.IsNotExist(err), "sidecar destination published: %v", err)
		data, err := os.ReadFile(dst + suffix)
		envOK(t, err == nil && string(data) == "old sidecar", "sidecar changed: %q (%v)", data, err)
	}
}

func TestBootstrapDBTruthAndUnsupportedLink(t *testing.T) {
	src, _ := bootstrapSource(t)
	dst := freshDest(t)
	tp := filepath.Join(paths.StateDir(), "memory", "truth")
	envOK(t, os.MkdirAll(filepath.Dir(tp), 0o755) == nil && os.WriteFile(tp, []byte("db\n"), 0o644) == nil, "set db truth")
	orig := linkNoReplace
	t.Cleanup(func() { linkNoReplace = orig })
	linkNoReplace = func(_, _ string) error { return errors.ErrUnsupported }
	_, err := BootstrapCopy(context.Background(), src, dst)
	envOK(t, errors.Is(err, errors.ErrUnsupported), "unsupported link: %v", err)
	_, err = os.Lstat(dst)
	envOK(t, os.IsNotExist(err), "unsupported link published: %v", err)
	linkNoReplace = orig
	_, err = BootstrapCopy(context.Background(), src, dst)
	envOK(t, err == nil, "db-truth bootstrap: %v", err)
}
