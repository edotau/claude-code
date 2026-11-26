package shadow

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/sqlite"
)

// memorySteps is the memory store's migration list; step n sets user_version n.
var memorySteps = []sqlite.Step{{Name: "schema", Up: schemaV1}}

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

func schemaV1(tx *sql.Tx) (string, error) {
	for _, s := range schemaV1Stmts {
		if _, err := tx.ExecContext(context.Background(), s); err != nil {
			return "", fmt.Errorf("memory: shadow schema: %w", err)
		}
	}
	return "8 tables", nil
}

// migrateMemoryDB brings db to the latest memory schema.
func migrateMemoryDB(ctx context.Context, db *sql.DB, opts sqlite.MigrateOptions) (sqlite.Report, error) {
	if opts.Name == "" {
		opts.Name = "memory"
	}
	return sqlite.Migrate(ctx, db, memorySteps, opts)
}

// ShadowDBPath is the shadow store; paths.Protected keeps clean away from it and its sidecars.
func ShadowDBPath() string { return filepath.Join(paths.ConfigDir(), "state", "memory.db") }

// ShadowReport is one rebuild: rows written and the files whose render from rows is not byte-equal.
type ShadowReport struct {
	Docs, Entries int
	Mismatched    []string
}

// RebuildShadow migrates the shadow db (snapshot in state/backups first) and rebuilds it from the memory store.
func RebuildShadow(ctx context.Context) (ShadowReport, error) {
	path := ShadowDBPath()
	db, err := sqlite.Open(path, sqlite.Options{})
	if err != nil {
		return ShadowReport{}, fmt.Errorf("memory: shadow open: %w", err)
	}
	defer db.Close()
	opts := sqlite.MigrateOptions{SnapshotDir: filepath.Join(filepath.Dir(path), "backups")}
	if _, err := migrateMemoryDB(ctx, db, opts); err != nil {
		return ShadowReport{}, fmt.Errorf("memory: shadow migrate: %w", err)
	}
	return shadowRebuild(ctx, db, paths.MemoryStoreDir())
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
}

var (
	grammarNames = map[memory.Grammar]string{memory.GrammarDated: "dated", memory.GrammarBullets: "bullets", memory.GrammarSections: "sections"}
	kindNames    = map[memory.Kind]string{memory.KindHeading: "heading", memory.KindBullet: "bullet"}
)

var (
	shadowNoteDirs   = []string{"feedback", "notes", "techdebt", "knowledge"}
	shadowRecordDirs = []string{"reports", "handoffs", "worktrees"}
)

// shadowClear empties every table keyed by entry rowid too: a rebuild reassigns rowids.
var shadowClear = []string{"DELETE FROM vectors", "DELETE FROM conflicts", "DELETE FROM changes",
	"DELETE FROM entries", "DELETE FROM docs"}

// shadowRebuild replaces every docs and entries row with root's files, then checks each render is byte-equal.
func shadowRebuild(ctx context.Context, db *sql.DB, root string) (ShadowReport, error) {
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() { // a missing root must never empty the db
		return ShadowReport{}, fmt.Errorf("memory: shadow rebuild: store root %s is not a directory (%v)", root, err)
	}
	docs, err := shadowDocs(root)
	if err != nil {
		return ShadowReport{}, err
	}
	var n ShadowReport
	err = sqlite.Tx(ctx, db, func(tx *sql.Tx) error {
		n = ShadowReport{}
		for _, q := range shadowClear {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		for _, d := range docs {
			if err := insertShadowDoc(ctx, tx, d); err != nil {
				return err
			}
			n.Docs, n.Entries = n.Docs+1, n.Entries+len(d.entries)
		}
		return nil
	})
	if err != nil {
		return ShadowReport{}, fmt.Errorf("memory: shadow rebuild: %w", err)
	}
	n.Mismatched, err = shadowMismatches(ctx, db, docs)
	return n, err
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

func insertShadowDoc(ctx context.Context, tx *sql.Tx, d shadowDoc) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO docs(path, kind, header, hidden, grammar, frontmatter)
		VALUES(?, ?, ?, ?, ?, ?)`, d.path, d.kind, d.header, d.hidden, d.grammar, d.front); err != nil {
		return err
	}
	for _, e := range d.entries {
		if _, err := tx.ExecContext(ctx, `INSERT INTO entries(doc, id, seq, kind, lines, date, area, version,
			deleted, origin, origin_seq, rendered_rev) VALUES(?, ?, ?, ?, ?, ?, '', 1, 0, '', 0, 1)`,
			d.path, e.id, e.seq, e.kind, e.lines, e.date); err != nil {
			return err
		}
	}
	return nil
}

// shadowDocs reads and parses every in-scope file under root, sorted by path.
func shadowDocs(root string) ([]shadowDoc, error) {
	var docs []shadowDoc
	for _, name := range memory.BankFiles() {
		d, ok, err := readShadowDoc(root, name, "bank", true)
		if err != nil {
			return nil, err
		}
		if ok {
			docs = append(docs, d)
		}
	}
	more, err := scopedDocs(root)
	if err != nil {
		return nil, err
	}
	docs = append(docs, more...)
	slices.SortFunc(docs, func(a, b shadowDoc) int { return strings.Compare(a.path, b.path) })
	return docs, nil
}

// scopedDocs is the archive, note and record files; a missing folder contributes nothing.
func scopedDocs(root string) ([]shadowDoc, error) {
	var docs []shadowDoc
	add := func(rel, kind string, perEntry bool) error {
		d, ok, err := readShadowDoc(root, rel, kind, perEntry)
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

// readShadowDoc reads rel; ok is false for a missing bank file.
func readShadowDoc(root, rel, kind string, perEntry bool) (shadowDoc, bool, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if errors.Is(err, fs.ErrNotExist) && kind == "bank" { // a walked file that vanished must fail, not drop its rows
		return shadowDoc{}, false, nil
	}
	if err != nil {
		return shadowDoc{}, false, fmt.Errorf("memory: shadow read %s: %w", rel, err)
	}
	doc := memory.ParseDoc(rel, data)
	d := shadowDoc{path: rel, kind: kind, header: doc.Header, front: joinedOrNil(doc.Front, doc.Front == nil), raw: data}
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
func renderShadowDoc(ctx context.Context, db *sql.DB, path string) ([]byte, error) {
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
