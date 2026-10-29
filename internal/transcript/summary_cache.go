package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
)

// summaryMinAge is how old a file's mtime must be before its summary is cached; younger files are still being appended.
const summaryMinAge = time.Minute

// summaryCacheFile is transcript-summary.json on disk: one row per absolute transcript path.
type summaryCacheFile struct {
	Schema int                   `json:"schema"`
	Files  map[string]summaryRow `json:"files"`
}

type summaryRow struct {
	Size    int64       `json:"size"`
	MtimeNS int64       `json:"mtime_ns"`
	Summary summaryJSON `json:"summary"`
}

// summaryJSON is FileSummary with its unexported per-minute input map exported, so the peak round-trips.
type summaryJSON struct {
	Models     map[string]*ModelUsage `json:"models"`
	Turns      int                    `json:"turns"`
	ModelTurns map[string]int         `json:"model_turns"`
	First      time.Time              `json:"first"`
	Last       time.Time              `json:"last"`
	PerMin     map[int64]int64        `json:"per_min"`
}

func toSummaryJSON(s *FileSummary) summaryJSON {
	return summaryJSON{Models: s.Models, Turns: s.Turns, ModelTurns: s.ModelTurns, First: s.First, Last: s.Last, PerMin: s.perMin}
}

// fileSummary rebuilds a FileSummary with its own maps, so a caller cannot alter the cached row.
func (j summaryJSON) fileSummary() *FileSummary {
	s := &FileSummary{Models: map[string]*ModelUsage{}, Turns: j.Turns, ModelTurns: map[string]int{}, First: j.First, Last: j.Last,
		perMin: map[int64]int64{}}
	for k, v := range j.Models {
		if v != nil {
			u := *v
			s.Models[k] = &u
		}
	}
	for k, v := range j.ModelTurns {
		s.ModelTurns[k] = v
	}
	for k, v := range j.PerMin {
		s.perMin[k] = v
	}
	return s
}

// SummaryCachePath is the one transcript-summary cache file, scoped by CLAUDE_CONFIG_DIR like StateDir.
func SummaryCachePath() string {
	return filepath.Join(paths.StateDir(), "transcript", "summary.json")
}

// SummaryCache memoizes SummarizeFile per absolute path across runs, keyed by size and mtime; not concurrent-safe.
type SummaryCache struct {
	path         string
	old, cur     map[string]summaryRow
	hits, misses int
}

// OpenSummaryCache loads the cache at path; a missing, corrupt or other-version file reads as empty, never an error.
func OpenSummaryCache(path string) *SummaryCache {
	c := &SummaryCache{path: path, old: map[string]summaryRow{}, cur: map[string]summaryRow{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var f summaryCacheFile
	if schemas.DecodeOK(summaryCacheFormat, raw, &f) && f.Files != nil {
		c.old = f.Files
	}
	return c
}

// Stats reports how many Summarize calls were served from the cache and how many re-read the file.
func (c *SummaryCache) Stats() (hits, misses int) { return c.hits, c.misses }

// Summarize is SummarizeFile through the cache: a row whose size and mtime match, on a file older than a minute, wins.
func (c *SummaryCache) Summarize(path string) (*FileSummary, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	st, statErr := os.Stat(path) // stat before reading: a racing append then misses next run
	if statErr != nil {
		return SummarizeFile(path)
	}
	size, mtime := st.Size(), st.ModTime().UnixNano()
	settled := time.Since(st.ModTime()) > summaryMinAge
	if row, ok := c.old[abs]; ok && settled && row.Size == size && row.MtimeNS == mtime {
		c.hits++
		c.cur[abs] = row
		return row.Summary.fileSummary(), nil
	}
	s, err := SummarizeFile(path)
	if err != nil {
		return nil, err
	}
	c.misses++
	if settled {
		c.cur[abs] = summaryRow{Size: size, MtimeNS: mtime, Summary: toSummaryJSON(s)}
	}
	return s, nil
}

// Save writes the rows seen this run atomically, dropping the rest; it skips the write when nothing changed.
func (c *SummaryCache) Save() error {
	if c.misses == 0 && len(c.cur) == len(c.old) {
		return nil
	}
	raw, err := json.Marshal(summaryCacheFile{Schema: summaryCacheSchema, Files: c.cur})
	if err != nil {
		return err
	}
	return paths.AtomicWrite(c.path, raw, 0o600)
}

// AddFileCached is AddFile through c.
func (a *UsageAggregate) AddFileCached(c *SummaryCache, path string) error {
	s, err := c.Summarize(path)
	if err != nil {
		return err
	}
	a.Add(s)
	return nil
}
