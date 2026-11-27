package shadow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/sqlite"
)

func init() {
	memory.OpenExportDB = openExportDB
}

// exportBeforeRebuild runs under the bank lock; a conflict refuses the rebuild (the db side is kept beside the file).
func exportBeforeRebuild() error {
	edb, closeDB, err := openExportDB()
	if err != nil {
		return err
	}
	rep, err := memory.ExportGuardLocked(edb, paths.MemoryStoreDir())
	err = errors.Join(err, closeDB())
	if err == nil && len(rep.Conflicts) > 0 {
		return fmt.Errorf("memory: export conflicts %v: list them with `memory shadow --conflicts`, then rebuild again", rep.Conflicts)
	}
	return err
}

type exportDB struct {
	db    *sql.DB
	root  string
	moved map[int64]bool
}

func openExportDB() (memory.ExportDB, func() error, error) {
	path := ShadowDBPath()
	db, err := sqlite.Open(path, sqlite.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("memory: export db open: %w", err)
	}
	opts := sqlite.MigrateOptions{SnapshotDir: filepath.Join(filepath.Dir(path), "backups")}
	if _, err := migrateMemoryDB(context.Background(), db, opts); err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("memory: export db migrate: %w", err)
	}
	return &exportDB{db: db, root: paths.MemoryStoreDir()}, db.Close, nil
}

func (e *exportDB) Walk() ([]string, error) {
	var out []string
	for _, name := range memory.BankFiles() {
		if fi, err := os.Stat(filepath.Join(e.root, name)); err == nil && !fi.IsDir() {
			out = append(out, name)
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("memory: export walk %s: %w", name, err)
		}
	}
	archives, err := walkMD(e.root, "archive", false)
	out = append(out, archives...)
	for _, dirs := range [][]string{shadowNoteDirs, shadowRecordDirs} {
		for _, dir := range dirs {
			rels, werr := walkMD(e.root, dir, true)
			err = errors.Join(err, werr)
			out = append(out, rels...)
		}
	}
	if err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}

func (e *exportDB) Paths() ([]string, error) {
	rows, err := e.db.QueryContext(context.Background(), `SELECT path FROM docs ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("memory: export paths: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("memory: export paths: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (e *exportDB) Render(rel string) ([]byte, error) {
	return renderShadowDoc(context.Background(), e.db, rel)
}

func (e *exportDB) Rendered(rel string) (string, error) {
	var val string
	err := e.db.QueryRowContext(context.Background(), `SELECT value FROM meta WHERE key = ?`, "render:"+rel).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("memory: export rendered %s: %w", rel, err)
	}
	return val, nil
}

func (e *exportDB) MarkRendered(rel, sum string) error {
	_, err := e.db.ExecContext(context.Background(), `INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, "render:"+rel, sum)
	if err != nil {
		return fmt.Errorf("memory: export mark %s: %w", rel, err)
	}
	if err := e.stampExported(rel, sum); err != nil {
		return err
	}
	return execEntryCount(context.Background(), e.db)
}

// execEntryCount keeps meta 'entries' at the live row count, which row recall checks against its imported count.
func execEntryCount(ctx context.Context, x interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}) error {
	_, err := x.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES('entries',
		(SELECT count(*) FROM entries WHERE deleted = 0))`)
	return err
}

// stampExported records the fingerprint of the file the guard just wrote, so the next open does not re-import it.
func (e *exportDB) stampExported(rel, sum string) error {
	full := filepath.Join(e.root, filepath.FromSlash(rel))
	fi, err := os.Stat(full) // before the read, as readShadowDoc stamps: a racing edit only costs a re-import
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(full)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != sum {
		return nil // not the bytes just rendered: leave the import to see it
	}
	stamp := fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano())
	if _, err := e.db.ExecContext(context.Background(), `INSERT INTO meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, "stat:"+rel, stamp); err != nil {
		return fmt.Errorf("memory: export stamp %s: %w", rel, err)
	}
	return nil
}

// RecordConflict files an unresolved row in conflicts (entry_rowid 0, kind and doc columns) once.
func (e *exportDB) RecordConflict(rel, kind string) error {
	_, err := e.db.ExecContext(context.Background(), `INSERT INTO conflicts(entry_rowid, kind, doc, at, resolved)
		SELECT 0, ?1, ?2, ?3, 0 WHERE NOT EXISTS (SELECT 1 FROM conflicts WHERE entry_rowid = 0
		AND kind = ?1 AND doc = ?2 AND resolved = 0)`, kind, rel, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("memory: export conflict %s: %w", rel, err)
	}
	return nil
}

func (e *exportDB) Import(rel, wantSum string) error {
	kind, perEntry, err := scopeForRel(rel)
	if err != nil {
		return err
	}
	d, ok, err := readShadowDoc(e.root, rel, kind, perEntry, nil)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("memory: export import %s: missing file", rel)
	}
	ctx := context.Background()
	sum := sha256.Sum256(d.raw)
	return sqlite.Tx(ctx, e.db, func(tx *sql.Tx) error {
		if err := checkImportBase(ctx, tx, rel, wantSum); err != nil {
			return err
		}
		if err := e.upsertImportDoc(ctx, tx, d); err != nil {
			return err
		}
		for k, v := range map[string]string{"render:" + rel: hex.EncodeToString(sum[:]), "stat:" + rel: d.stamp} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES(?, ?)
				ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
				return err
			}
		}
		return execEntryCount(ctx, tx)
	})
}

// checkImportBase refuses when rel's db render no longer has wantSum ("" means the doc must still be absent).
func checkImportBase(ctx context.Context, tx *sql.Tx, rel, wantSum string) error {
	if wantSum == "" {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM docs WHERE path = ?`, rel).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			return memory.ErrExportStale
		}
		return nil
	}
	cur, err := renderShadowDoc(ctx, tx, rel)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(cur)
	if hex.EncodeToString(sum[:]) != wantSum {
		return memory.ErrExportStale
	}
	return nil
}

func scopeForRel(rel string) (string, bool, error) {
	if slices.Contains(memory.BankFiles(), rel) {
		return "bank", true, nil
	}
	if strings.HasPrefix(rel, "archive/") {
		_, mapped := memory.ArchiveSource(filepath.Base(rel))
		return "archive", mapped, nil
	}
	top, _, _ := strings.Cut(rel, "/")
	if slices.Contains(shadowNoteDirs, top) {
		return "note", false, nil
	}
	if slices.Contains(shadowRecordDirs, top) {
		return "record", false, nil
	}
	return "", false, fmt.Errorf("memory: export import %s: outside scope", rel)
}

func (e *exportDB) upsertImportDoc(ctx context.Context, tx *sql.Tx, d shadowDoc) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO docs(path, kind, header, hidden, grammar, frontmatter)
		VALUES(?, ?, ?, ?, ?, ?) ON CONFLICT(path) DO UPDATE SET kind = excluded.kind, header = excluded.header,
		hidden = excluded.hidden, grammar = excluded.grammar, frontmatter = excluded.frontmatter`,
		d.path, d.kind, d.header, d.hidden, d.grammar, d.front); err != nil {
		return err
	}
	oldRows, err := loadLiveEntries(ctx, tx, d.path)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE entries SET seq = -rowid WHERE doc = ?`, d.path); err != nil {
		return err
	}
	rowids := matchRows(oldRows, d.entries)
	oldLines, err := e.claimMovedRows(ctx, tx, d.path, oldRows, d.entries, rowids)
	if err != nil {
		return err
	}
	used := make(map[int64]bool, len(oldLines))
	for i, en := range d.entries {
		if rowid := rowids[i]; rowid != 0 {
			used[rowid] = true
			if err := updateMatched(ctx, tx, d.path, en, rowid, oldLines[rowid] != canonText(en.lines)); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO entries(doc, id, seq, kind, lines, date, area, version)
			VALUES(?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(version), 0) + 1 FROM entries))`,
			d.path, en.id, en.seq, en.kind, en.lines, en.date, deriveArea(en.lines)); err != nil {
			return err
		}
	}
	if err := deleteUnused(ctx, tx, oldRows, used, e.moved); err != nil {
		return err
	}
	return nil
}

// claimMovedRows lets an unmatched id-marked entry reuse a unique row with the same id whose old doc dropped it.
func (e *exportDB) claimMovedRows(ctx context.Context, tx *sql.Tx, doc string, oldRows []liveEntry, entries []shadowEntry, rowids []int64) (map[int64]string, error) {
	if e.moved == nil {
		e.moved = map[int64]bool{}
	}
	oldLines := make(map[int64]string, len(oldRows))
	for _, r := range oldRows {
		oldLines[r.rowid] = canonText(r.lines)
	}
	count := map[string]int{}
	for _, en := range entries {
		count[en.id]++
	}
	for i, en := range entries {
		if rowids[i] != 0 || en.id == "" || count[en.id] != 1 {
			continue
		}
		rowid, ok, err := e.findMovedRow(ctx, tx, doc, en.id)
		if err != nil {
			return nil, err
		}
		if ok {
			rowids[i], oldLines[rowid] = rowid, "\x00" // a doc move always counts as edited so its change is captured
		}
	}
	return oldLines, nil
}

// findMovedRow claims the one entries row with id in another doc when that doc already dropped it or lacks it on disk.
func (e *exportDB) findMovedRow(ctx context.Context, tx *sql.Tx, doc, id string) (int64, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT rowid, doc, deleted FROM entries WHERE id = ? AND doc <> ?`, id, doc)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	var rowid int64
	var src string
	var del, n int
	for rows.Next() {
		if err := rows.Scan(&rowid, &src, &del); err != nil {
			return 0, false, err
		}
		n++
	}
	if err := rows.Err(); err != nil || n != 1 {
		return 0, false, err
	}
	ok := e.moved[rowid]
	if del == 0 {
		ok = e.missingFromDocFile(src, id)
	}
	if !ok {
		return 0, false, nil
	}
	delete(e.moved, rowid)
	_, err = tx.ExecContext(ctx, `UPDATE entries SET seq = -rowid WHERE rowid = ?`, rowid)
	return rowid, err == nil, err
}

// missingFromDocFile reports whether src on disk lacks id (or is gone), so a later import of src will drop it.
func (e *exportDB) missingFromDocFile(src, id string) bool {
	kind, perEntry, err := scopeForRel(src)
	if err != nil || !perEntry {
		return false
	}
	d, ok, err := readShadowDoc(e.root, src, kind, perEntry, nil)
	if err != nil {
		return false
	}
	return !ok || !slices.ContainsFunc(d.entries, func(en shadowEntry) bool { return en.id == id })
}

// deleteUnused marks every old row the file no longer holds as deleted and records id-marked ones for a later doc.
func deleteUnused(ctx context.Context, tx *sql.Tx, oldRows []liveEntry, used, moved map[int64]bool) error {
	for _, r := range oldRows {
		if used[r.rowid] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE entries SET deleted = 1, version = version + 1 WHERE rowid = ?`, r.rowid); err != nil {
			return err
		}
		if r.id != "" {
			moved[r.rowid] = true
		}
	}
	return nil
}

// updateMatched rewrites a kept row; a doc move un-deletes an earlier guard-dropped row.
func updateMatched(ctx context.Context, tx *sql.Tx, doc string, e shadowEntry, rowid int64, edited bool) error {
	_, err := tx.ExecContext(ctx, `UPDATE entries SET doc = ?9, id = ?1, seq = ?2, kind = ?3, date = ?4, area = ?5, deleted = 0,
		version = version + ((?8 OR (id <> '' AND ?1 <> '' AND id <> ?1) OR kind <> ?3 OR date <> ?4) AND deleted = 0), lines = ?6 WHERE rowid = ?7`,
		e.id, e.seq, e.kind, e.date, deriveArea(e.lines), e.lines, rowid, edited, doc)
	return err
}

// matchRows gives each entry its kept row (0 = new): same text, a lone edited id, then the same id by position.
func matchRows(old []liveEntry, entries []shadowEntry) []int64 {
	m := rowMatch{old: old, entries: entries, out: make([]int64, len(entries)), taken: make([]bool, len(old))}
	for pass := range 5 {
		for i, e := range entries {
			if ok := m.rule(pass, i, e); ok != nil {
				m.take(i, ok)
			}
		}
	}
	return m.out
}

// loneID matches an entry whose id is held once on each side.
func (m rowMatch) loneID(i int, e shadowEntry) func(liveEntry) bool {
	if m.out[i] != 0 || e.id == "" || m.countOld(e.id) != 1 || m.countNew(e.id) != 1 {
		return nil
	}
	return func(r liveEntry) bool { return r.id == e.id }
}

// rule is pass's row test for entry i: exact text, text without id marks or wrapping, a lone id, then id by position.
func (m rowMatch) rule(pass, i int, e shadowEntry) func(liveEntry) bool {
	switch pass {
	case 0:
		return func(r liveEntry) bool { return r.id == e.id && r.lines == e.lines }
	case 1:
		return func(r liveEntry) bool { return r.id == e.id && canonText(r.lines) == canonText(e.lines) }
	case 2:
		return m.loneID(i, e)
	case 3: // an id stamp on an unstamped row
		return func(r liveEntry) bool { return r.id == "" && canonText(r.lines) == canonText(e.lines) }
	}
	return func(r liveEntry) bool { return r.id == e.id && r.seq == int64(e.seq) }
}

type rowMatch struct {
	old     []liveEntry
	entries []shadowEntry
	out     []int64
	taken   []bool
}

// take gives entry i the first free old row that ok accepts.
func (m rowMatch) take(i int, ok func(liveEntry) bool) {
	for j, r := range m.old {
		if m.out[i] == 0 && !m.taken[j] && ok(r) {
			m.out[i], m.taken[j] = r.rowid, true
		}
	}
}

func (m rowMatch) countOld(id string) (n int) {
	for j, r := range m.old {
		if !m.taken[j] && r.id == id {
			n++
		}
	}
	return n
}

func (m rowMatch) countNew(id string) (n int) {
	for k, f := range m.entries {
		if m.out[k] == 0 && f.id == id {
			n++
		}
	}
	return n
}

type liveEntry struct {
	rowid, seq int64
	id, lines  string
}

func loadLiveEntries(ctx context.Context, tx *sql.Tx, doc string) ([]liveEntry, error) {
	rows, err := tx.QueryContext(ctx, `SELECT rowid, seq, id, lines FROM entries WHERE doc = ? AND deleted = 0 ORDER BY seq`, doc)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []liveEntry
	for rows.Next() {
		var r liveEntry
		if err := rows.Scan(&r.rowid, &r.seq, &r.id, &r.lines); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ExportConflicts lists the unresolved export conflicts ("kind doc time") and every kept copy ("copy rel").
func ExportConflicts() ([]string, error) {
	edb, closeDB, err := openExportDB()
	if err != nil {
		return nil, err
	}
	defer func() { _ = closeDB() }()
	e := edb.(*exportDB)
	rows, err := e.db.QueryContext(context.Background(),
		`SELECT kind || ' ' || doc || ' ' || at FROM conflicts WHERE entry_rowid = 0 AND resolved = 0 ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("memory: export conflicts: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("memory: export conflicts: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	copies, err := keptCopies(memory.HeldDir())
	for _, c := range copies {
		out = append(out, "copy "+c)
	}
	return out, err
}

func keptCopies(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if err != nil || d.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if _, ok := memory.KeptCopyBase(filepath.ToSlash(rel)); ok && rerr == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return rerr
	})
	return out, err
}

// ClearExportConflict deletes the kept copy id (a held-dir path) and resolves its conflicts.
func ClearExportConflict(id string) error {
	clean := path.Clean(id)
	if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("memory: export clear: %q is not a store-relative path", id)
	}
	edb, closeDB, err := openExportDB()
	if err != nil {
		return err
	}
	defer func() { _ = closeDB() }()
	e := edb.(*exportDB)
	base, isCopy := memory.KeptCopyBase(clean)
	if isCopy {
		if err := removeKeptCopy(memory.HeldDir(), clean); err != nil {
			return fmt.Errorf("memory: export clear %s: %w", clean, err)
		}
	}
	_, err = e.db.ExecContext(context.Background(),
		`UPDATE conflicts SET resolved = 1 WHERE entry_rowid = 0 AND resolved = 0 AND doc IN (?, ?)`, clean, base)
	if err != nil {
		return fmt.Errorf("memory: export clear %s: %w", clean, err)
	}
	return nil
}

// removeKeptCopy deletes rel when it is a regular file; os.Root keeps every step inside root, symlinks included.
func removeKeptCopy(root, rel string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	fi, err := r.Lstat(rel)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return r.Remove(rel)
}
