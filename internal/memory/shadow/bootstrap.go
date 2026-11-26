package shadow

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/sqlite"
)

// linkNoReplace publishes the finished copy; a seam so tests can make dst appear mid-call.
var linkNoReplace = os.Link

// BootstrapCopy installs src at the current store's ShadowDBPath, with a new identity and one db-only export.
func BootstrapCopy(ctx context.Context, src, dst string) (string, error) {
	active, err := filepath.Abs(ShadowDBPath())
	if err != nil {
		return "", err
	}
	dest, err := filepath.Abs(dst)
	if err != nil || dest != active {
		return "", fmt.Errorf("memory bootstrap: destination must be %s", active)
	}
	if err := checkBootstrapDest(); err != nil {
		return "", err
	}
	var id string
	err = memory.WithBankLock(memory.BankLockWait, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := checkBootstrapDest(); err != nil {
			return err
		}
		var err error
		id, err = copyPeer(ctx, src, dst)
		if err != nil {
			return err
		}
		// SetTruth takes this same non-reentrant lock; bootstrap already validated the stored truth above.
		truth := filepath.Join(paths.StateDir(), "memory", "truth")
		if err := paths.AtomicWrite(truth, []byte("db\n"), 0o644); err != nil {
			return fmt.Errorf("memory bootstrap: copy published at %s, set truth: %w", dst, err)
		}
		if err := exportCopy(dst); err != nil {
			return fmt.Errorf("memory bootstrap: copy published at %s, export: %w", dst, err)
		}
		return nil
	})
	return id, err
}

// checkBootstrapDest refuses a store whose stored truth is not db or missing, or whose dir already holds bank files.
func checkBootstrapDest() error {
	b, err := os.ReadFile(filepath.Join(paths.StateDir(), "memory", "truth"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("memory bootstrap: read truth: %w", err)
	case strings.TrimSpace(string(b)) != "db":
		return fmt.Errorf("memory bootstrap: destination truth is %q: only a db or missing truth can be bootstrapped", strings.TrimSpace(string(b)))
	}
	for _, name := range append(memory.BankFiles(), "MEMORY.md", "index.md", "catalog.md") {
		p := filepath.Join(paths.MemoryStoreDir(), name)
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("memory bootstrap: destination store already holds %s", p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("memory bootstrap: %w", err)
		}
	}
	return nil
}

type bootstrapExportDB struct{ *exportDB }

// Walk considers only copied docs, so unrelated destination files never become input.
func (e bootstrapExportDB) Walk() ([]string, error) { return nil, nil }

func (e bootstrapExportDB) Import(rel, _ string) error {
	return fmt.Errorf("memory bootstrap: refusing to import destination file %s", rel)
}

// exportCopy renders the store's Markdown once; the caller holds the bank lock.
func exportCopy(dbPath string) error {
	db, err := sqlite.Open(dbPath, sqlite.Options{})
	if err != nil {
		return err
	}
	edb := &exportDB{db: db, root: paths.MemoryStoreDir()}
	if err := os.MkdirAll(edb.root, 0o700); err != nil {
		return errors.Join(err, db.Close())
	}
	rep, err := memory.ExportGuardLocked(bootstrapExportDB{edb}, edb.root)
	if err == nil && len(rep.Conflicts) > 0 {
		err = fmt.Errorf("export conflicts %v", rep.Conflicts)
	}
	return errors.Join(err, db.Close())
}

// copyPeer copies src to dst with a new machine_id, via an owned temp linked in without replacing.
func copyPeer(ctx context.Context, src, dst string) (string, error) {
	for _, p := range []string{dst, dst + "-wal", dst + "-shm"} {
		if _, err := os.Lstat(p); err == nil {
			return "", fmt.Errorf("memory bootstrap: %s already exists", p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("memory bootstrap: destination: %w", err)
		}
	}
	f, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("memory bootstrap: temp: %w", err)
	}
	tmp := f.Name()
	defer func() { // only files this call created
		_ = os.Remove(tmp)
		_ = os.Remove(tmp + "-wal")
		_ = os.Remove(tmp + "-shm")
	}()
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := vacuumCopy(ctx, src, tmp); err != nil {
		return "", err
	}
	id, err := mintMachineID(ctx, tmp)
	if err != nil {
		return "", err
	}
	if err := linkNoReplace(tmp, dst); err != nil {
		return "", fmt.Errorf("memory bootstrap: publish: %w", err)
	}
	if err := paths.SyncDir(filepath.Dir(dst)); err != nil {
		return id, fmt.Errorf("memory bootstrap: copy published at %s, sync dir: %w", dst, err)
	}
	return id, nil
}

func validateCopy(ctx context.Context, path string) (err error) {
	db, err := sqlite.Open(path, sqlite.Options{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	v, err := sqlite.Version(db)
	if err != nil {
		return err
	}
	if v != SchemaVersion() {
		return fmt.Errorf("memory bootstrap: source schema %d, want %d: migrate source first", v, SchemaVersion())
	}
	rows, err := db.QueryContext(ctx, `SELECT path FROM docs`)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			return err
		}
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			return fmt.Errorf("memory bootstrap: invalid copied document path %q", rel)
		}
		p := filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(rel))
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("memory bootstrap: destination already holds %s", p)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return rows.Err()
}

// vacuumCopy writes a consistent single-file copy of src to dst (dst 0600).
func vacuumCopy(ctx context.Context, src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("memory bootstrap: source: %w", err)
	}
	db, err := sqlite.Open(src, sqlite.Options{}) // VACUUM INTO needs a writable handle on a WAL db
	if err != nil {
		return fmt.Errorf("memory bootstrap: open source: %w", err)
	}
	_, err = db.ExecContext(ctx, `VACUUM INTO ?`, dst)
	if err = errors.Join(err, db.Close()); err != nil {
		return fmt.Errorf("memory bootstrap: copy: %w", err)
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		return err
	}
	return validateCopy(ctx, dst)
}
