package shadow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/sqlite"
)

// sqlBankStore is the bank seam over the shadow db: one row per entry; a Version is the row's counter, never reused.
type sqlBankStore struct{ db *sql.DB }

func init() { memory.ShadowImport = importShadow }

// importShadow imports changed docs and lists the divergent ones; changed means that set moved since meta diverged.
func importShadow() (docs []string, changed bool, err error) {
	if _, err := os.Stat(ShadowDBPath()); err != nil {
		if errors.Is(err, fs.ErrNotExist) { // no shadow yet: nothing verified, nothing logged
			return nil, false, nil
		}
		return nil, false, err
	}
	ctx := context.Background()
	db, err := openMigratedShadow(ctx)
	if err != nil {
		return nil, false, err
	}
	defer db.Close()
	report, err := shadowSync(ctx, db, paths.MemoryStoreDir(), false)
	docs = report.Mismatched
	slices.Sort(docs)
	var prev string
	qerr := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'diverged'`).Scan(&prev)
	if qerr != nil && !errors.Is(qerr, sql.ErrNoRows) {
		return docs, false, errors.Join(err, qerr)
	}
	now := strings.Join(docs, "\n")
	if changed = now != prev; changed { // stored even on a failed import, so a partial set logs once
		_, serr := db.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES('diverged', ?)`, now)
		err = errors.Join(err, serr)
	}
	return docs, changed, err
}

var _ memory.BankStore = sqlBankStore{}

// NewSQLBankStore returns the seam over db; nothing but tests uses it until the truth flips (plan 7.1).
func NewSQLBankStore(db *sql.DB) memory.BankStore { return sqlBankStore{db: db} }

// entryRow is one live entries row with the ids a write needs.
type entryRow struct {
	rowid   int64
	version int
	entry   memory.DocEntry
}

// bankState is the bank as the rows hold it: docs, their rows in seq order, and each ref's Version.
type bankState struct {
	docs     map[string]memory.Doc
	rows     map[string][]entryRow
	versions memory.Versions
}

func (s sqlBankStore) Load() (map[string]memory.Doc, memory.Versions, error) {
	ctx := context.Background()
	st := bankState{}
	err := sqlite.Tx(ctx, s.db, func(tx *sql.Tx) (err error) {
		st, err = loadBank(ctx, tx)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return st.docs, st.versions, nil
}

// Apply runs b in one transaction; a stale base (*memory.ConflictError) or a failed op writes nothing.
func (s sqlBankStore) Apply(b memory.Batch, base memory.Versions, now time.Time) ([]memory.OpResult, error) {
	ctx := context.Background()
	var results []memory.OpResult
	err := sqlite.Tx(ctx, s.db, func(tx *sql.Tx) error {
		st, err := loadBank(ctx, tx)
		if err != nil {
			return err
		}
		if err := checkBase(b, base, st.versions); err != nil {
			return err
		}
		var next map[string]memory.Doc
		next, results, err = memory.ApplyDocs(st.docs, b, now)
		if err != nil {
			return err
		}
		return writeBank(ctx, tx, st, next)
	})
	return results, err
}

// checkBase refuses the first op on an entry that is ambiguous, has no base to check, or moved past its base.
func checkBase(b memory.Batch, base, cur memory.Versions) error {
	for i, o := range b.Ops {
		have, known := cur[o.ID]
		want := base[o.ID]
		unchecked := want == "" && (o.Read != "" || strings.HasPrefix(o.ID, "z"))
		if o.ID == "" || (!known && want == "") || (unchecked && have != "") || (want != "" && have == want) {
			continue
		}
		return &memory.ConflictError{Op: i, Ref: o.ID, Base: want, Current: have}
	}
	return nil
}

func loadBank(ctx context.Context, tx *sql.Tx) (bankState, error) {
	st := bankState{docs: map[string]memory.Doc{}, rows: map[string][]entryRow{}, versions: memory.Versions{}}
	for _, name := range memory.BankFiles() {
		d, err := loadDoc(ctx, tx, name)
		if err != nil {
			return bankState{}, err
		}
		st.docs[name] = d.Doc
		st.rows[name] = d.rows
		for _, r := range d.rows {
			ref := r.entry.Ref()
			if _, seen := st.versions[ref]; seen {
				st.versions[ref] = ""
			} else {
				st.versions[ref] = memory.Version(strconv.Itoa(r.version))
			}
		}
	}
	return st, nil
}

type loadedDoc struct {
	memory.Doc
	rows []entryRow
}

// loadDoc rebuilds one bank file's Doc from its rows; a file with no docs row is an empty doc.
func loadDoc(ctx context.Context, tx *sql.Tx, name string) (loadedDoc, error) {
	var header, grammar string
	var hidden, front sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT header, hidden, frontmatter, grammar FROM docs WHERE path = ?`, name).
		Scan(&header, &hidden, &front, &grammar)
	if err == sql.ErrNoRows {
		return loadedDoc{Doc: memory.ParseDoc(name, nil)}, nil
	}
	if err != nil {
		return loadedDoc{}, fmt.Errorf("memory: sql store load %s: %w", name, err)
	}
	d := memory.ParseDoc(name, nil)
	d.Header, d.Hidden, d.Front = header, splitOrNil(hidden), splitOrNil(front)
	rows, err := tx.QueryContext(ctx, `SELECT rowid, id, seq, kind, lines, date, version FROM entries
		WHERE doc = ? AND deleted = 0 ORDER BY seq`, name)
	if err != nil {
		return loadedDoc{}, fmt.Errorf("memory: sql store load %s: %w", name, err)
	}
	defer rows.Close()
	out := loadedDoc{Doc: d}
	for rows.Next() {
		var r entryRow
		var kind, lines string
		e := &r.entry
		if err := rows.Scan(&r.rowid, &e.ID, &e.Seq, &kind, &lines, &e.Date, &r.version); err != nil {
			return loadedDoc{}, fmt.Errorf("memory: sql store load %s: %w", name, err)
		}
		e.Kind, e.Lines, e.Row = kindOf(kind), strings.Split(lines, "\n"), r.rowid
		out.Entries = append(out.Entries, *e)
		out.rows = append(out.rows, r)
	}
	return out, rows.Err()
}

func splitOrNil(s sql.NullString) []string {
	if !s.Valid {
		return nil
	}
	return strings.Split(s.String, "\n")
}

func kindOf(name string) memory.Kind {
	for k, n := range kindNames {
		if n == name {
			return k
		}
	}
	return memory.KindBullet
}

// writeBank stores every bank doc that differs from its rows: kept entries bump version only when changed.
func writeBank(ctx context.Context, tx *sql.Tx, st bankState, next map[string]memory.Doc) error {
	for _, name := range memory.BankFiles() {
		if err := writeDoc(ctx, tx, name, st, next[name]); err != nil {
			return fmt.Errorf("memory: sql store write %s: %w", name, err)
		}
	}
	return nil
}

func writeDoc(ctx context.Context, tx *sql.Tx, name string, st bankState, now memory.Doc) error {
	old := st.rows[name]
	if string(st.docs[name].Render()) == string(now.Render()) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO docs(path, kind, header, hidden, grammar, frontmatter)
		VALUES(?, 'bank', ?, ?, ?, ?) ON CONFLICT(path) DO UPDATE SET header = excluded.header,
		hidden = excluded.hidden, frontmatter = excluded.frontmatter`,
		name, now.Header, joinedOrNil(now.Hidden, len(now.Hidden) == 0), grammarNames[now.Grammar],
		joinedOrNil(now.Front, now.Front == nil)); err != nil {
		return err
	}
	// free every seq first: the (doc, seq) index is unique and entries move
	if _, err := tx.ExecContext(ctx, `UPDATE entries SET seq = -rowid WHERE doc = ?`, name); err != nil {
		return err
	}
	kept := map[int64]bool{}
	for i, e := range now.Entries {
		kept[e.Row] = true
		if err := writeEntry(ctx, tx, slot{name, i, e.Row}, e); err != nil {
			return err
		}
	}
	for _, r := range old {
		if !kept[r.rowid] {
			if _, err := tx.ExecContext(ctx, `UPDATE entries SET deleted = 1, version = version + 1 WHERE rowid = ?`, r.rowid); err != nil {
				return err
			}
		}
	}
	return nil
}

// slot is where an entry lands: its doc, its position, and the row it keeps (0 for a new row).
type slot struct {
	doc   string
	seq   int
	rowid int64
}

// writeEntry updates the kept row (version +1 when its lines changed) or inserts a new one.
func writeEntry(ctx context.Context, tx *sql.Tx, at slot, e memory.DocEntry) error {
	lines := strings.Join(e.Lines, "\n")
	if at.rowid == 0 {
		_, err := tx.ExecContext(ctx, `INSERT INTO entries(doc, id, seq, kind, lines, date, version, rendered_rev)
			VALUES(?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(version), 0) + 1 FROM entries), 0)`, at.doc, e.ID, at.seq, kindNames[e.Kind], lines, e.Date)
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE entries SET id = ?, seq = ?, kind = ?, date = ?,
		version = version + (lines <> ?), lines = ? WHERE rowid = ?`,
		e.ID, at.seq, kindNames[e.Kind], e.Date, lines, lines, at.rowid)
	return err
}
