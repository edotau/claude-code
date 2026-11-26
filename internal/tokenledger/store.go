// Package tokenledger is the per-token ledger the proxy appends to and usage and the status line read.
package tokenledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/schemas"
	"github.com/edotau/claude-code/internal/textutil"
)

// Entry is one metered proxy request
type Entry struct {
	Schema   int       `json:"schema"`
	Ts       time.Time `json:"ts"`
	Profile  string    `json:"profile,omitempty"`
	Provider string    `json:"provider"`
	Agent    string    `json:"agent,omitempty"`
	Session  string    `json:"session,omitempty"`
	Model    string    `json:"model"`
	// Requested is the model the client asked for, set only when the proxy served a different one.
	Requested string `json:"requested,omitempty"`
	// AskedModel is the body's model string, Call the request kind, Route the asked /p/<profile> when another served it.
	AskedModel string `json:"asked_model,omitempty"`
	Call       string `json:"call,omitempty"`
	Route      string `json:"route,omitempty"`
	Input      int    `json:"input"`
	Output     int    `json:"output"`
	CacheRead  int    `json:"cache_read"`
	// CacheWrite is every cache-write token (5m and 1h); CacheWrite1h is the 1h part, 0 when unsplit.
	CacheWrite   int `json:"cache_write,omitempty"`
	CacheWrite1h int `json:"cache_write_1h,omitempty"`
	// FirstMs is send to first client byte, TotalMs send to the ledger write, both of the attempt that served.
	FirstMs int64 `json:"first_ms,omitempty"`
	TotalMs int64 `json:"total_ms,omitempty"`
	// CostUSD is the served model's owner price, else its list price; 0 when neither names one.
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// Path is <config dir>/state/usage/ledger.jsonl.
func Path() string {
	return filepath.Join(paths.ConfigDir(), "state", "usage", "ledger.jsonl")
}

// Append appends one JSON line; best-effort — a write failure never fails the caller's request.
func Append(e Entry) {
	e.Schema = schemaVersion
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	_ = paths.AppendLineRepair(Path(), b)
}

// Read reads every entry, oldest first; a missing ledger file is empty, not an error.
func Read() ([]Entry, error) {
	var out []Entry
	err := readMatching(nil, 0, func(e Entry) { out = append(out, e) })
	return out, err
}

// readMatching feeds fn the lines containing sub (nil: all) in the last tail bytes (0: all), a cut first line skipped.
func readMatching(sub []byte, tail int64, fn func(Entry)) error {
	f, err := os.Open(Path())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	skip := false
	if tail > 0 {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		size := info.Size()
		off := max(0, size-tail)
		r, skip = io.NewSectionReader(f, off, size-off), off > 0
	}
	return textutil.EachLineErr(r, func(line []byte) {
		if skip {
			skip = false
			return
		}
		if !bytes.Contains(line, sub) {
			return
		}
		var e Entry
		if _, err := schemas.Decode(format, line, &e); err == nil {
			fn(e)
		}
	})
}

// ProfileName is the registry row that served the entry; pre-attribution lines stored it in Provider.
func (e Entry) ProfileName() string {
	if e.Profile != "" {
		return e.Profile
	}
	return e.Provider
}

type tally struct {
	in, out, cacheRead int
	usd                float64
}

type sessionLedger struct {
	info   os.FileInfo
	offset int64
	totals map[string]tally
	days   map[string]float64
}

var sessionLedgers = struct {
	sync.Mutex
	files map[string]*sessionLedger
}{files: make(map[string]*sessionLedger)}

// SessionTotals sums tokens stamped with the parent session id; an empty id matches nothing.
func SessionTotals(id string) (in, out, cacheRead int, err error) {
	if id == "" {
		return 0, 0, 0, nil
	}
	t, _, err := totalsFor(id, "")
	return t.in, t.out, t.cacheRead, err
}

// SpendTotals is the USD stamped on session's rows and on every row of now's local day; "" counts only the day.
func SpendTotals(session string, now time.Time) (sessionUSD, dayUSD float64, err error) {
	t, day, err := totalsFor(session, dayKey(now))
	return t.usd, day, err
}

func dayKey(t time.Time) string { return t.In(time.Local).Format(time.DateOnly) }

// totalsFor returns id's tally and day's USD, the unterminated tail line included.
func totalsFor(id, day string) (t tally, d float64, err error) {
	path := Path()
	sessionLedgers.Lock()
	defer sessionLedgers.Unlock()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		delete(sessionLedgers.files, path)
		return tally{}, 0, nil
	}
	if err != nil {
		return tally{}, 0, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return tally{}, 0, err
	}
	c := sessionLedgers.files[path]
	if !c.reusable(info) {
		c = &sessionLedger{totals: make(map[string]tally), days: make(map[string]float64)}
		sessionLedgers.files[path] = c
	}
	t, d, err = c.readTail(f, info.Size(), id, day)
	if err != nil {
		delete(sessionLedgers.files, path)
	}
	c.info = info
	return t, d, err
}

func (c *sessionLedger) reusable(info os.FileInfo) bool {
	return c != nil && os.SameFile(c.info, info) && info.Size() >= c.info.Size() &&
		(info.Size() != c.info.Size() || info.ModTime().Equal(c.info.ModTime()))
}

func (c *sessionLedger) readTail(f *os.File, size int64, id, day string) (tally, float64, error) {
	var tail tally
	var tailDay float64
	if c.offset != size {
		_, err := textutil.ScanRawLines(io.NewSectionReader(f, c.offset, size-c.offset), false, func(line []byte) {
			complete := line[len(line)-1] == '\n'
			if complete {
				c.offset += int64(len(line))
			}
			var e Entry
			if _, err := schemas.Decode(format, line, &e); err != nil {
				return
			}
			// An unterminated entry counts now but must be re-read if the next write extends it.
			if !complete {
				if e.Session != "" && e.Session == id {
					tail = tally{e.Input, e.Output, e.CacheRead, e.CostUSD}
				}
				if dayKey(e.Ts) == day {
					tailDay = e.CostUSD
				}
				return
			}
			c.days[dayKey(e.Ts)] += e.CostUSD
			if e.Session == "" {
				return
			}
			sum := c.totals[e.Session]
			c.totals[e.Session] = tally{sum.in + e.Input, sum.out + e.Output, sum.cacheRead + e.CacheRead, sum.usd + e.CostUSD}
		})
		if err != nil {
			return tally{}, 0, err
		}
	}
	sum := c.totals[id]
	return tally{sum.in + tail.in, sum.out + tail.out, sum.cacheRead + tail.cacheRead, sum.usd + tail.usd},
		c.days[day] + tailDay, nil
}

const schemaVersion = 1

var format = schemas.Register(schemas.Format{
	Name: "usage-ledger", Owner: "internal/tokenledger", Kind: schemas.Log,
	Glob:    Path,
	New:     func() any { return &Entry{} },
	Version: schemaVersion, Keep: "usage history is the ledger of record", Migrate: schemas.SameShape,
})
