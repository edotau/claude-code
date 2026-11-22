package hooktelemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

// foldEntry counts one hook's sub-millisecond passes inside the pending minute.
type foldEntry struct {
	Event  string  `json:"event"`
	Module string  `json:"module"`
	N      int     `json:"n"`
	Max    float64 `json:"max"`
}

// foldState is the pending minute of counters, kept beside the day file until the minute rolls over.
type foldState struct {
	Minute string            `json:"minute"`
	Rows   []foldEntry       `json:"rows"`
	Cache  []paths.CacheStat `json:"cache,omitempty"` // summed per cache name across the folded passes
}

func foldable(r Row) bool {
	return r.Outcome == OutcomeOK && r.Dur < time.Millisecond && r.Rule == "" && r.Verdict == ""
}

func foldPath(file string) string { return strings.TrimSuffix(file, ".jsonl") + ".fold" }

func loadFold(file string) foldState {
	var st foldState
	if data, err := os.ReadFile(foldPath(file)); err == nil {
		if json.Unmarshal(data, &st) != nil {
			st = foldState{}
		}
	}
	return st
}

func saveFold(file string, st foldState) {
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	tmp := foldPath(file) + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, foldPath(file))
	}
}

// add returns st with the row counted.
func (st foldState) add(r Row) foldState {
	ms := float64(r.Dur) / float64(time.Millisecond)
	rows := append([]foldEntry(nil), st.Rows...)
	for i, e := range rows {
		if e.Event == r.Event && e.Module == r.Module {
			rows[i].N++
			rows[i].Max = max(e.Max, ms)
			return foldState{st.Minute, rows, st.Cache}
		}
	}
	return foldState{st.Minute, append(rows, foldEntry{r.Event, r.Module, 1, ms}), st.Cache}
}

// addCache returns st with stats summed into its per-name cache counters.
func (st foldState) addCache(stats []paths.CacheStat) foldState {
	cache := append([]paths.CacheStat(nil), st.Cache...)
	for _, c := range stats {
		i := 0
		for i < len(cache) && cache[i].Name != c.Name {
			i++
		}
		if i == len(cache) {
			cache = append(cache, paths.CacheStat{Name: c.Name})
		}
		cache[i].Hits += c.Hits
		cache[i].Misses += c.Misses
		cache[i].Refreshes += c.Refreshes
		cache[i].Evictions += c.Evictions
	}
	sort.Slice(cache, func(i, j int) bool { return cache[i].Name < cache[j].Name })
	return foldState{st.Minute, st.Rows, cache}
}

func (st foldState) cacheSummary() string {
	parts := make([]string, len(st.Cache))
	for i, c := range st.Cache {
		parts[i] = fmt.Sprintf("%s=%d/%d/%d/%d", c.Name, c.Hits, c.Misses, c.Refreshes, c.Evictions)
	}
	return strings.Join(parts, " ")
}

// flushLines renders the pending counters as one row each, stamped with their own minute.
func (st foldState) flushLines() [][]byte {
	var out [][]byte
	cache := st.cacheSummary()
	for _, e := range st.Rows {
		line, err := json.Marshal(telemetryLine{T: st.Minute, Event: e.Event, Module: e.Module, Ms: e.Max, Outcome: OutcomeOK, N: e.N, Cache: cache})
		if err == nil {
			cache = "" // the minute's cache sums ride the first row only
			out = append(out, append(line, '\n'))
		}
	}
	return out
}

// rollFold loads the pending counters and, when their minute is over, flushes them to the day file.
func rollFold(day *telemetryFile, file string, now time.Time) foldState {
	st := loadFold(file)
	minute := now.Truncate(time.Minute).Format(time.RFC3339)
	if st.Minute == minute {
		return st
	}
	for _, l := range st.flushLines() {
		if appendTelemetry(day, l) != nil {
			break
		}
	}
	return foldState{Minute: minute}
}
