package transcript

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// summaryMinAge is how old a file's mtime must be before its turns are cached; younger files are still being appended.
const summaryMinAge = time.Minute

// summaryCacheFile is transcript/summary.json on disk: one row per absolute transcript path.
type summaryCacheFile struct {
	Schema int                   `json:"schema"`
	Files  map[string]summaryRow `json:"files"`
}

type summaryRow struct {
	Size    int64     `json:"size"`
	MtimeNS int64     `json:"mtime_ns"`
	Turns   []turnRow `json:"turns"`
}

// turnRow is one Response, compact: model, the four token counts and the stamp in unix ns (0 when unstamped).
type turnRow struct {
	M  string   `json:"m"`
	U  [4]int64 `json:"u"`
	At int64    `json:"t,omitempty"`
}

func toTurnRows(resps []Response) []turnRow {
	rows := make([]turnRow, len(resps))
	for i, r := range resps {
		u := r.Usage
		rows[i] = turnRow{M: r.Model, U: [4]int64{u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens}}
		if !r.At.IsZero() {
			rows[i].At = r.At.UnixNano()
		}
	}
	return rows
}

// responses rebuilds a fresh Response slice, so a caller cannot alter the cached row.
func (row summaryRow) responses() []Response {
	resps := make([]Response, len(row.Turns))
	for i, t := range row.Turns {
		resps[i] = Response{Model: t.M, Usage: ModelUsage{t.U[0], t.U[1], t.U[2], t.U[3]}}
		if t.At != 0 {
			resps[i].At = time.Unix(0, t.At)
		}
	}
	return resps
}

// SummaryCachePath is the one transcript-summary cache file, scoped by CLAUDE_CONFIG_DIR like StateDir.
func SummaryCachePath() string {
	return filepath.Join(paths.StateDir(), "transcript", "summary.json")
}

// SummaryCache memoizes ReadResponses per absolute path across runs, keyed by size and mtime; one goroutine each.
type SummaryCache struct {
	path         string
	old, cur     map[string]summaryRow
	hits, misses int
	dirty        bool // a settled miss stored a row, so Save has something new to write
}

// OpenSummaryCache loads the cache at path; a missing, corrupt or other-version file reads as empty, never an error.
func OpenSummaryCache(path string) *SummaryCache {
	return &SummaryCache{path: path, old: readSummaryRows(path), cur: map[string]summaryRow{}}
}

// readSummaryRows decodes the cache file's rows; a missing, corrupt or other-version file is an empty map.
func readSummaryRows(path string) map[string]summaryRow {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]summaryRow{}
	}
	var f summaryCacheFile
	if !schemas.DecodeOK(summaryCacheFormat, raw, &f) || f.Files == nil {
		return map[string]summaryRow{}
	}
	return f.Files
}

// Stats reports how many Responses calls were served from the cache and how many re-read the file.
func (c *SummaryCache) Stats() (hits, misses int) { return c.hits, c.misses }

// Responses is ReadResponses through the cache: a row whose size and mtime match, on a file older than a minute, wins.
func (c *SummaryCache) Responses(path string) ([]Response, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	st, statErr := os.Stat(path) // stat before reading: a racing append then misses next run
	if statErr != nil {
		return ReadResponses(path)
	}
	size, mtime := st.Size(), st.ModTime().UnixNano()
	settled := time.Since(st.ModTime()) > summaryMinAge
	if row, ok := c.old[abs]; ok && settled && row.Size == size && row.MtimeNS == mtime {
		c.hits++
		c.cur[abs] = row
		return row.responses(), nil
	}
	resps, err := ReadResponses(path)
	if err != nil {
		return nil, err
	}
	c.misses++
	if settled {
		c.dirty = true
		c.cur[abs] = summaryRow{Size: size, MtimeNS: mtime, Turns: toTurnRows(resps)}
	}
	return resps, nil
}

// Save merges this run's rows into the file's current rows under its lock, so a concurrent run's rows survive.
func (c *SummaryCache) Save() error {
	return paths.WithFileLock(c.path, func() error {
		disk := readSummaryRows(c.path)
		rows := make(map[string]summaryRow, len(disk)+len(c.cur))
		for k, v := range disk {
			if _, seen := c.cur[k]; seen {
				continue
			}
			if _, err := os.Stat(k); !errors.Is(err, fs.ErrNotExist) { // a gone file prunes its row; the cache stays bounded
				rows[k] = v
			}
		}
		maps.Copy(rows, c.cur)
		if !c.dirty && len(rows) == len(disk) {
			return nil
		}
		raw, err := json.Marshal(summaryCacheFile{Schema: summaryCacheSchema, Files: rows})
		if err != nil {
			return err
		}
		return paths.AtomicWrite(c.path, raw, 0o600)
	})
}

// summaryCacheSchema is the summary cache's current schema version; a mismatch is a miss, never migrated.
const summaryCacheSchema = 2 // 2: per-turn rows, so usage --days can cut by timestamp

var summaryCacheFormat = schemas.Register(schemas.Format{
	Name: "transcript-summary", Owner: "internal/transcript", Kind: schemas.Cache,
	Glob:    SummaryCachePath,
	New:     func() any { return &summaryCacheFile{} },
	Version: summaryCacheSchema, Keep: "rebuildable cache; a version mismatch is a miss",
})
