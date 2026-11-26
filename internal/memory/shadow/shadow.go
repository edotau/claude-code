package shadow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/sqlite"
)

// memorySteps is the memory store's migration list; step n sets user_version n.
var memorySteps = []sqlite.Step{{Name: "schema", Up: schemaV1}, {Name: "area", Up: areaV2},
	{Name: "conflict-kind", Up: conflictKindV3}, {Name: "vectors", Up: vectorsV4}, {Name: "entry-uid", Up: entryUIDV5}}

// schemaV1Stmts is schema v1, one statement per Exec so a failure names its statement.
var schemaV1Stmts = []string{
	`CREATE TABLE docs(path TEXT PRIMARY KEY, kind TEXT NOT NULL, header TEXT NOT NULL, hidden TEXT,
		grammar TEXT NOT NULL, frontmatter TEXT)`,
	`CREATE TABLE entries(rowid INTEGER PRIMARY KEY, doc TEXT NOT NULL REFERENCES docs(path), id TEXT NOT NULL,
		seq INTEGER NOT NULL, kind TEXT NOT NULL, lines TEXT NOT NULL, date TEXT NOT NULL, area TEXT NOT NULL DEFAULT '',
		version INTEGER NOT NULL, deleted INTEGER NOT NULL DEFAULT 0, origin TEXT NOT NULL DEFAULT '',
		origin_seq INTEGER NOT NULL DEFAULT 0, rendered_rev INTEGER NOT NULL DEFAULT 0)`,
	`CREATE INDEX entries_doc_id ON entries(doc, id)`,
	`CREATE UNIQUE INDEX entries_doc_seq ON entries(doc, seq)`,
	`CREATE TABLE changes(origin TEXT, origin_seq INTEGER, schema INTEGER, entry_rowid INTEGER, base_version TEXT,
		op TEXT, payload TEXT, at TEXT, PRIMARY KEY(origin, origin_seq))`,
	`CREATE TABLE conflicts(entry_rowid INTEGER, peer_origin TEXT, peer_seq INTEGER, payload TEXT, at TEXT,
		resolved INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE peers(origin TEXT PRIMARY KEY, acked_seq INTEGER NOT NULL DEFAULT 0, schema INTEGER NOT NULL DEFAULT 0)`,
	`CREATE VIRTUAL TABLE entries_fts USING fts5(lines, area, doc, content='entries', content_rowid='rowid')`,
	`CREATE TRIGGER entries_ai AFTER INSERT ON entries BEGIN
		INSERT INTO entries_fts(rowid, lines, area, doc) VALUES(new.rowid, new.lines, new.area, new.doc); END`,
	`CREATE TRIGGER entries_ad AFTER DELETE ON entries BEGIN
		INSERT INTO entries_fts(entries_fts, rowid, lines, area, doc) VALUES('delete', old.rowid, old.lines, old.area, old.doc); END`,
	`CREATE TRIGGER entries_au AFTER UPDATE ON entries BEGIN
		INSERT INTO entries_fts(entries_fts, rowid, lines, area, doc) VALUES('delete', old.rowid, old.lines, old.area, old.doc);
		INSERT INTO entries_fts(rowid, lines, area, doc) VALUES(new.rowid, new.lines, new.area, new.doc); END`,
	`CREATE TABLE vectors(entry_rowid INTEGER, model TEXT, dim INTEGER, vec BLOB, minted_at TEXT,
		PRIMARY KEY(entry_rowid, model))`,
	`CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT)`,
}

func conflictKindV3(tx *sql.Tx) (string, error) {
	for _, s := range []string{`ALTER TABLE conflicts ADD COLUMN kind TEXT NOT NULL DEFAULT 'envelope'`,
		`ALTER TABLE conflicts ADD COLUMN doc TEXT`,
		`UPDATE conflicts SET kind = peer_origin, doc = payload WHERE entry_rowid = 0 AND peer_origin <> ''`} {
		if _, err := tx.ExecContext(context.Background(), s); err != nil {
			return "", err
		}
	}
	return "conflict kind and doc", nil
}

func schemaV1(tx *sql.Tx) (string, error) {
	for _, s := range schemaV1Stmts {
		if _, err := tx.ExecContext(context.Background(), s); err != nil {
			return "", fmt.Errorf("memory: shadow schema: %w", err)
		}
	}
	return "8 tables", nil
}

var citedPkg = regexp.MustCompile(`\b(?:internal|cmd)/([A-Za-z0-9_-]+)`)

// deriveArea is the most-cited package or command dir in lines, ties to the smaller name; "" when none (R2).
func deriveArea(lines string) string {
	count := map[string]int{}
	best := ""
	for _, m := range citedPkg.FindAllStringSubmatch(lines, -1) {
		count[m[1]]++
		if c, b := count[m[1]], count[best]; c > b || (c == b && m[1] < best) {
			best = m[1]
		}
	}
	return best
}

// areaV2 fills the empty area of every entry from the paths it cites.
func areaV2(tx *sql.Tx) (string, error) {
	ctx := context.Background()
	rows, err := tx.QueryContext(ctx, `SELECT rowid, lines FROM entries WHERE area = '' ORDER BY rowid`)
	if err != nil {
		return "", fmt.Errorf("memory: shadow area: %w", err)
	}
	type upd struct {
		id   int64
		area string
	}
	var areas []upd
	for rows.Next() {
		var id int64
		var lines string
		if err := rows.Scan(&id, &lines); err != nil {
			_ = rows.Close()
			return "", fmt.Errorf("memory: shadow area: %w", err)
		}
		if a := deriveArea(lines); a != "" {
			areas = append(areas, upd{id, a})
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", fmt.Errorf("memory: shadow area: %w", err)
	}
	for _, u := range areas {
		if _, err := tx.ExecContext(ctx, `UPDATE entries SET area = ? WHERE rowid = ?`, u.area, u.id); err != nil {
			return "", fmt.Errorf("memory: shadow area: %w", err)
		}
	}
	return fmt.Sprintf("%d areas", len(areas)), nil
}

// migrateMemoryDB brings db to the latest memory schema.
func migrateMemoryDB(ctx context.Context, db *sql.DB, opts sqlite.MigrateOptions) (sqlite.Report, error) {
	if opts.Name == "" {
		opts.Name = "memory"
	}
	return sqlite.Migrate(ctx, db, memorySteps, opts)
}

// SchemaVersion is the newest memory.db version this build knows.
func SchemaVersion() int { return len(memorySteps) }

// ShadowDBPath is the shadow store; paths.Protected keeps clean away from it and its sidecars.
func ShadowDBPath() string { return filepath.Join(paths.ConfigDir(), "state", "memory.db") }

// ShadowReport is one rebuild: rows written and the files whose render from rows is not byte-equal.
type ShadowReport struct {
	Docs, Entries int
	Mismatched    []string
}

// RebuildShadow migrates the shadow db (snapshot in state/backups first) and rebuilds it from the memory store.
func RebuildShadow(ctx context.Context) (rep ShadowReport, err error) {
	err = memory.WithBankLock(0, func() error { // an older rebuild must not publish baselines after a newer one
		if memory.Truth() == "db" { // db-only changes must reach the Markdown before it replaces the db
			if xerr := exportBeforeFlip(); xerr != nil {
				return fmt.Errorf("rebuild refused, export under db truth failed: %w", xerr)
			}
		}
		db, oerr := openMigratedShadow(ctx)
		if oerr != nil {
			return oerr
		}
		defer db.Close()
		rep, oerr = shadowRebuild(ctx, db, paths.MemoryStoreDir())
		return oerr
	})
	return rep, err
}

func openMigratedShadow(ctx context.Context) (*sql.DB, error) {
	path := ShadowDBPath()
	db, err := sqlite.Open(path, sqlite.Options{})
	if err != nil {
		return nil, fmt.Errorf("memory: shadow open: %w", err)
	}
	opts := sqlite.MigrateOptions{SnapshotDir: filepath.Join(filepath.Dir(path), "backups")}
	if _, err := migrateMemoryDB(ctx, db, opts); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("memory: shadow migrate: %w", err)
	}
	return db, nil
}

// shadowEntry is one entries row before insert.
type shadowEntry struct {
	id, kind, lines, date string
	seq                   int
}

// shadowDoc is one docs row and its entries; hidden and front are nil for SQL NULL.
type shadowDoc struct {
	path, kind, header, grammar string
	hidden, front               *string
	entries                     []shadowEntry
	raw                         []byte // the file as read, for the round-trip check
	same                        bool   // unchanged since its recorded fingerprint: not parsed, not imported
	stamp                       string // size:mtime, read before the file
}

var (
	grammarNames = map[memory.Grammar]string{memory.GrammarDated: "dated", memory.GrammarBullets: "bullets", memory.GrammarSections: "sections"}
	kindNames    = map[memory.Kind]string{memory.KindHeading: "heading", memory.KindBullet: "bullet"}
)

var (
	shadowNoteDirs   = []string{"feedback", "notes", "techdebt", "knowledge"}
	shadowRecordDirs = []string{"reports", "handoffs", "worktrees"}
)

// shadowClear empties rowid-keyed rows; uid-keyed changes, conflicts and peers stay (pruning changes is forbidden).
var shadowClear = []string{"DELETE FROM vectors", "DELETE FROM conflicts WHERE entry_rowid <> 0 AND entry_uid IS NULL",
	keepTombstones + ` WHERE deleted = 1`, "DELETE FROM entries", "DELETE FROM docs"}

// shadowRebuild replaces every docs and entries row with root's files, then checks each render is byte-equal.
func shadowRebuild(ctx context.Context, db *sql.DB, root string) (ShadowReport, error) {
	return shadowSync(ctx, db, root, true)
}

const (
	metaImported = "imported_schema"
	// renderVersion is bumped when ParseDoc or renderShadowDoc changes, so every doc is imported again.
	renderVersion = "1"
)

func importMarker() string { return strconv.Itoa(len(memorySteps)) + ":" + renderVersion }

// shadowSync imports root's files, only the docs whose stat fingerprint moved unless full (new db, moved schema).
func shadowSync(ctx context.Context, db *sql.DB, root string, full bool) (ShadowReport, error) {
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() { // a missing root must never empty the db
		return ShadowReport{}, fmt.Errorf("memory: shadow rebuild: store root %s is not a directory (%v)", root, err)
	}
	known, err := readFingerprints(ctx, db, full)
	if err != nil {
		return ShadowReport{}, err
	}
	docs, err := shadowDocs(root, known)
	if err != nil {
		return ShadowReport{}, err
	}
	var n ShadowReport
	var changed []shadowDoc
	err = sqlite.Tx(ctx, db, func(tx *sql.Tx) (err error) {
		n, changed, err = syncDocs(ctx, tx, docs, known == nil)
		return err
	})
	if err != nil {
		return ShadowReport{}, fmt.Errorf("memory: shadow rebuild: %w", err)
	}
	n.Mismatched, err = shadowMismatches(ctx, db, changed)
	if err != nil {
		return n, err
	}
	return n, writeFingerprints(ctx, db, changed, n.Mismatched)
}

// syncDocs clears what the import replaces (everything when full) and inserts every changed doc.
func syncDocs(ctx context.Context, tx *sql.Tx, docs []shadowDoc, full bool) (ShadowReport, []shadowDoc, error) {
	var n ShadowReport
	var changed []shadowDoc
	ids, err := shadowUIDs(ctx, tx) // read before the clear, so re-inserted entries keep their uid and base
	if err != nil {
		return n, nil, err
	}
	ids.adopt = full
	ids.claimMoved(docs)
	if full {
		for _, q := range append(slices.Clone(shadowClear), "DELETE FROM meta WHERE key LIKE 'stat:%' OR key LIKE 'render:%'") {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return n, nil, err
			}
		}
	} else if err := dropStale(ctx, tx, docs); err != nil {
		return n, nil, err
	}
	for _, d := range docs {
		if d.same {
			continue
		}
		if err := insertShadowDoc(ctx, tx, d, ids); err != nil {
			return n, nil, err
		}
		changed = append(changed, d)
		n.Docs, n.Entries = n.Docs+1, n.Entries+len(d.entries)
	}
	return n, changed, ids.tombstoneGone(ctx, tx)
}

// readFingerprints maps imported paths to their "size:mtime_ns" (meta stat:<path>); nil means import everything.
func readFingerprints(ctx context.Context, db *sql.DB, full bool) (map[string]string, error) {
	if full {
		return nil, nil
	}
	var v string
	err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaImported).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && v != importMarker()) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory: shadow meta: %w", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT substr(key, 6), value FROM meta WHERE key LIKE 'stat:%'`)
	if err != nil {
		return nil, fmt.Errorf("memory: shadow meta: %w", err)
	}
	defer rows.Close()
	known := map[string]string{}
	for rows.Next() {
		var p, h string
		if err := rows.Scan(&p, &h); err != nil {
			return nil, err
		}
		known[p] = h
	}
	return known, rows.Err()
}

// dropStale deletes the rows of every changed doc and of every doc row whose file is gone.
func dropStale(ctx context.Context, tx *sql.Tx, docs []shadowDoc) error {
	live := map[string]bool{}
	var drop []string
	for _, d := range docs {
		live[d.path] = true
		if !d.same {
			drop = append(drop, d.path)
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT path FROM docs`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			_ = rows.Close()
			return err
		}
		if !live[p] {
			drop = append(drop, p)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, p := range drop {
		for _, q := range dropDocStmts {
			if _, err := tx.ExecContext(ctx, q, p); err != nil {
				return err
			}
		}
	}
	return nil
}

// dropDocStmts remove one doc and its rowid-keyed rows (rowids are reused); uid-keyed changes and conflicts stay.
var dropDocStmts = []string{
	`DELETE FROM vectors WHERE entry_rowid IN (SELECT rowid FROM entries WHERE doc = ?1)`,
	`DELETE FROM conflicts WHERE entry_uid IS NULL AND entry_rowid IN (SELECT rowid FROM entries WHERE doc = ?1)`,
	keepTombstones + ` WHERE doc = ?1 AND deleted = 1`,
	`DELETE FROM entries WHERE doc = ?1`, `DELETE FROM docs WHERE path = ?1`, `DELETE FROM meta WHERE key IN ('stat:' || ?1, 'render:' || ?1)`,
}

// writeFingerprints records each imported doc whose render matched; a mismatched one is re-imported next time.
func writeFingerprints(ctx context.Context, db *sql.DB, changed []shadowDoc, mismatched []string) error {
	return sqlite.Tx(ctx, db, func(tx *sql.Tx) error {
		for _, d := range changed {
			q, args := `DELETE FROM meta WHERE key = 'render:' || ?`, []any{d.path}
			if !slices.Contains(mismatched, d.path) {
				sum := sha256.Sum256(d.raw)
				q, args = `INSERT OR REPLACE INTO meta(key, value) VALUES('render:' || ?, ?)`, []any{d.path, hex.EncodeToString(sum[:])}
			}
			if _, err := tx.ExecContext(ctx, q, args...); err != nil { // the imported bytes are the export baseline
				return err
			}
			if slices.Contains(mismatched, d.path) {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES('stat:' || ?, ?)`, d.path, d.stamp); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES(?, ?),
			('entries', (SELECT count(*) FROM entries WHERE deleted = 0))`, metaImported, importMarker())
		return err
	})
}

// shadowMismatches lists the docs whose render from rows differs from the bytes read.
func shadowMismatches(ctx context.Context, db *sql.DB, docs []shadowDoc) ([]string, error) {
	var out []string
	for _, d := range docs {
		got, err := renderShadowDoc(ctx, db, d.path)
		if err != nil {
			return out, err
		}
		if !bytes.Equal(got, d.raw) {
			out = append(out, d.path)
		}
	}
	return out, nil
}

// insertShadowDoc writes d's rows; rendered_rev 1 marks a plain import, which the capture triggers never record.
func insertShadowDoc(ctx context.Context, tx *sql.Tx, d shadowDoc, ids shadowIdentity) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO docs(path, kind, header, hidden, grammar, frontmatter)
		VALUES(?, ?, ?, ?, ?, ?)`, d.path, d.kind, d.header, d.hidden, d.grammar, d.front); err != nil {
		return err
	}
	for i, p := range ids.assign(d.path, d.entries) {
		e := d.entries[i]
		if _, err := tx.ExecContext(ctx, `INSERT INTO entries(doc, id, seq, kind, lines, date, area, uid, version,
			deleted, origin, origin_seq, rendered_rev) VALUES(?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, 0, ?, ?, 1)`,
			d.path, e.id, e.seq, e.kind, e.lines, e.date, deriveArea(e.lines), p.uid, p.version, p.origin, p.originSeq); err != nil {
			return err
		}
		if p.capture { // the version bump fires entries_capture_au with the old origin/seq as base
			if _, err := tx.ExecContext(ctx, `UPDATE entries SET version = version + 1 WHERE uid = ?`, p.uid); err != nil {
				return err
			}
		}
	}
	return nil
}

// shadowDocs reads and parses every in-scope file under root, sorted by path.
func shadowDocs(root string, known map[string]string) ([]shadowDoc, error) {
	var docs []shadowDoc
	for _, name := range memory.BankFiles() {
		d, ok, err := readShadowDoc(root, name, "bank", true, known)
		if err != nil {
			return nil, err
		}
		if ok {
			docs = append(docs, d)
		}
	}
	more, err := scopedDocs(root, known)
	if err != nil {
		return nil, err
	}
	docs = append(docs, more...)
	slices.SortFunc(docs, func(a, b shadowDoc) int { return strings.Compare(a.path, b.path) })
	return docs, nil
}

// scopedDocs is the archive, note and record files; a missing folder contributes nothing.
func scopedDocs(root string, known map[string]string) ([]shadowDoc, error) {
	var docs []shadowDoc
	add := func(rel, kind string, perEntry bool) error {
		d, ok, err := readShadowDoc(root, rel, kind, perEntry, known)
		if ok {
			docs = append(docs, d)
		}
		return err
	}
	archives, err := walkMD(root, "archive", false)
	for _, rel := range archives {
		_, mapped := memory.ArchiveSource(filepath.Base(rel))
		err = errors.Join(err, add(rel, "archive", mapped))
	}
	for kind, dirs := range map[string][]string{"note": shadowNoteDirs, "record": shadowRecordDirs} {
		for _, dir := range dirs {
			rels, werr := walkMD(root, dir, true)
			err = errors.Join(err, werr)
			for _, rel := range rels {
				err = errors.Join(err, add(rel, kind, false))
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return docs, nil
}

// walkMD lists dir's regular *.md files (recursive when deep) as root-relative slash paths, dot names skipped.
func walkMD(root, dir string, deep bool) ([]string, error) {
	var out []string
	base := filepath.Join(root, dir)
	if _, err := os.Lstat(base); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != base && (strings.HasPrefix(d.Name(), ".") || !deep) {
			return fs.SkipDir
		}
		if !shadowFile(d) {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		out = append(out, filepath.ToSlash(rel))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("memory: shadow walk %s: %w", dir, err)
	}
	return out, nil
}

// shadowGenerated names files rendered from rows, never imported in any folder.
var shadowGenerated = []string{"MEMORY.md", "index.md", "catalog.md"}

// shadowFile is a regular, non-dot, non-generated *.md file: dotfiles, *.lock and *.err never import.
func shadowFile(d fs.DirEntry) bool {
	n := d.Name()
	return d.Type().IsRegular() && !strings.HasPrefix(n, ".") && strings.HasSuffix(n, ".md") &&
		!slices.Contains(shadowGenerated, n)
}

// readShadowDoc reads rel (ok false for a missing bank file); a doc matching its recorded fingerprint is not parsed.
func readShadowDoc(root, rel, kind string, perEntry bool, known map[string]string) (shadowDoc, bool, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Stat(full)                              // stamped before the read, so a racing edit only costs a re-read
	if errors.Is(err, fs.ErrNotExist) && kind == "bank" { // a walked file that vanished must fail, not drop its rows
		return shadowDoc{}, false, nil
	}
	if err != nil {
		return shadowDoc{}, false, fmt.Errorf("memory: shadow read %s: %w", rel, err)
	}
	stamp := fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano())
	if known[rel] == stamp {
		return shadowDoc{path: rel, same: true}, true, nil
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return shadowDoc{}, false, fmt.Errorf("memory: shadow read %s: %w", rel, err)
	}
	doc := memory.ParseDoc(rel, data)
	d := shadowDoc{path: rel, kind: kind, header: doc.Header, front: joinedOrNil(doc.Front, doc.Front == nil), raw: data, stamp: stamp}
	if !perEntry {
		d.grammar = kind
		if kind == "archive" { // an unmapped archive file has no source grammar
			d.grammar = "file"
		}
		rest := slices.Clone(doc.Hidden)
		for _, e := range doc.Entries {
			rest = append(rest, e.Lines...)
		}
		if len(rest) > 0 {
			d.entries = []shadowEntry{{kind: "file", lines: strings.Join(rest, "\n")}}
		}
		return d, true, nil
	}
	d.grammar, d.hidden = grammarNames[doc.Grammar], joinedOrNil(doc.Hidden, len(doc.Hidden) == 0)
	for _, e := range doc.Entries {
		lines := strings.Join(e.Lines, "\n")
		d.entries = append(d.entries, shadowEntry{id: e.ID, kind: kindNames[e.Kind], lines: lines, date: e.Date, seq: e.Seq})
	}
	return d, true, nil
}

func joinedOrNil(lines []string, null bool) *string {
	if null {
		return nil
	}
	s := strings.Join(lines, "\n")
	return &s
}

// renderShadowQuery is one statement so the doc row and its entries come from one snapshot.
const renderShadowQuery = `SELECT d.header, d.hidden, d.frontmatter, e.lines FROM docs d
	LEFT JOIN entries e ON e.doc = d.path AND e.deleted = 0 WHERE d.path = ? ORDER BY e.seq`

// renderShadowDoc rebuilds path's bytes from its rows, joined exactly as Doc.Render joins.
func renderShadowDoc(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, path string) ([]byte, error) {
	rows, err := db.QueryContext(ctx, renderShadowQuery, path)
	if err != nil {
		return nil, fmt.Errorf("memory: shadow render %s: %w", path, err)
	}
	defer rows.Close()
	var parts []string
	found := false
	for rows.Next() {
		var header string
		var hidden, front, lines sql.NullString
		if err := rows.Scan(&header, &hidden, &front, &lines); err != nil {
			return nil, fmt.Errorf("memory: shadow render %s: %w", path, err)
		}
		if !found {
			parts, found = docHead(front, header, hidden), true
		}
		if lines.Valid {
			parts = append(parts, strings.Split(lines.String, "\n")...)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: shadow render %s: %w", path, err)
	}
	if !found {
		return nil, fmt.Errorf("memory: shadow render %s: %w", path, sql.ErrNoRows)
	}
	return []byte(strings.Join(parts, "\n")), nil
}

// docHead is the front, header and hidden lines in Doc.Render order; NULL means absent.
func docHead(front sql.NullString, header string, hidden sql.NullString) []string {
	var parts []string
	if front.Valid {
		parts = append(parts, strings.Split(front.String, "\n")...)
	}
	if header != "" {
		parts = append(parts, header)
	}
	if hidden.Valid {
		parts = append(parts, strings.Split(hidden.String, "\n")...)
	}
	return parts
}
