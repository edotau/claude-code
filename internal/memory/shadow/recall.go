package shadow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/sqlite"
)

func init() { memory.RowRecall = recallRows }

const recallReadTimeout = 250 * time.Millisecond

// recallRows opens read-only (memory.db is never created; WAL side files may appear beside an existing db).
func recallRows(terms []string) (map[string][]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), recallReadTimeout)
	defer cancel()
	stop := memory.RecallStage("current")
	files, ok := recallFiles(paths.MemoryStoreDir()) // the cheap stat check runs before the db opens
	stop()
	if !ok {
		return nil, false
	}
	stop = memory.RecallStage("open")
	db, err := sqlite.Open(ShadowDBPath(), sqlite.Options{ReadOnly: true, Busy: recallReadTimeout})
	stop()
	if err != nil {
		return nil, false
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, false
	}
	defer conn.Close()
	stop = memory.RecallStage("current")
	v1, v1OK := recallDataVersion(ctx, conn)
	current := recallCurrent(ctx, db, files)
	stop()
	if !current {
		return nil, false
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false
	}
	stop = memory.RecallStage("query")
	rendered, err := recallDocuments(ctx, tx, terms)
	_ = tx.Rollback() // Read-only rollback only releases the snapshot and its connection.
	stop()
	if err != nil {
		return nil, false
	}
	defer memory.RecallStage("current")()
	// Skip the second full scan when PRAGMA data_version on the same pinned connection confirms no peer committed.
	if v2, ok := recallDataVersion(ctx, conn); v1OK && ok && v1 == v2 {
		return rendered, true
	}
	return rendered, recallCurrent(ctx, db, files)
}

func recallDataVersion(ctx context.Context, conn *sql.Conn) (int64, bool) {
	var v int64
	return v, conn.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&v) == nil
}

func recallFiles(root string) (map[string]string, bool) {
	names := memory.BankFiles()
	for _, dir := range append(append([]string{"archive"}, shadowNoteDirs...), shadowRecordDirs...) {
		more, err := walkMD(root, dir, dir != "archive")
		if err != nil {
			return nil, false
		}
		names = append(names, more...)
	}
	infos := make([]os.FileInfo, len(names))
	errs := make([]error, len(names))
	eachParallel(len(names), func(i int) { infos[i], errs[i] = os.Stat(filepath.Join(root, filepath.FromSlash(names[i]))) })
	files := make(map[string]string, len(names))
	for i, name := range names {
		info, err := infos[i], errs[i]
		if errors.Is(err, os.ErrNotExist) && !strings.Contains(name, "/") {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || time.Since(info.ModTime()) < paths.RacyWindow {
			return nil, false
		}
		files[name] = fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
	}
	return files, true
}

func recallCurrent(ctx context.Context, db *sql.DB, files map[string]string) bool {
	known, err := readFingerprints(ctx, db, false)
	if err != nil || known == nil || len(known) != len(files) {
		return false
	}
	for path, stamp := range known {
		if files[path] != stamp {
			return false
		}
	}
	var count string
	var entries int
	if db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'entries'`).Scan(&count) != nil {
		return false
	}
	return db.QueryRowContext(ctx, `SELECT count(*) FROM entries WHERE deleted = 0`).Scan(&entries) == nil && count == strconv.Itoa(entries)
}

func recallDocuments(ctx context.Context, tx *sql.Tx, terms []string) (map[string][]byte, error) {
	encoded, err := json.Marshal(terms)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT d.path, d.header, d.hidden, d.frontmatter, e.lines FROM docs d
		LEFT JOIN entries e ON e.doc = d.path AND e.deleted = 0
		WHERE d.path IN (
			SELECT e.doc FROM entries_fts JOIN entries e ON e.rowid = entries_fts.rowid
			WHERE entries_fts MATCH ? AND e.deleted = 0
			UNION SELECT docs.path FROM docs, json_each(?) q
			WHERE instr(lower(coalesce(frontmatter, '') || header || coalesce(hidden, '')), q.value) > 0)
		ORDER BY d.path, e.seq`, recallMatch(terms), string(encoded))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	parts := map[string][]string{}
	for rows.Next() {
		var path, header string
		var hidden, front, lines sql.NullString
		if err := rows.Scan(&path, &header, &hidden, &front, &lines); err != nil {
			return nil, err
		}
		if _, found := parts[path]; !found {
			parts[path] = docHead(front, header, hidden)
		}
		if lines.Valid {
			parts[path] = append(parts[path], lines.String)
		}
	}
	docs := make(map[string][]byte, len(parts))
	for path, lines := range parts {
		docs[path] = []byte(strings.Join(lines, "\n"))
	}
	return docs, rows.Err()
}

func recallMatch(terms []string) string {
	query := make([]string, 0, len(terms))
	for _, term := range terms {
		if term != "" {
			query = append(query, `"`+strings.ReplaceAll(term, `"`, `""`)+`"*`)
		}
	}
	return strings.Join(query, " OR ")
}
