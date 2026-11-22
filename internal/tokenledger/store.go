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
	Input     int    `json:"input"`
	Output    int    `json:"output"`
	CacheRead int    `json:"cache_read"`
	// CacheWrite is every cache-write token (5m and 1h); CacheWrite1h is the 1h part, 0 when unsplit.
	CacheWrite   int `json:"cache_write,omitempty"`
	CacheWrite1h int `json:"cache_write_1h,omitempty"`
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
func Read() ([]Entry, error) { return readMatching(nil) }

// readMatching is Read decoding only lines that contain sub (nil keeps every line).
func readMatching(sub []byte) ([]Entry, error) {
	f, err := os.Open(Path())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Entry
	err = textutil.EachLineErr(f, func(line []byte) {
		if !bytes.Contains(line, sub) {
			return
		}
		var e Entry
		if _, err := schemas.Decode(format, line, &e); err == nil {
			out = append(out, e)
		}
	})
	return out, err
}

// ProfileName is the registry row that served the entry; pre-attribution lines stored it in Provider.
func (e Entry) ProfileName() string {
	if e.Profile != "" {
		return e.Profile
	}
	return e.Provider
}

type sessionLedger struct {
	info   os.FileInfo
	offset int64
	totals map[string][3]int
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
	path := Path()
	sessionLedgers.Lock()
	defer sessionLedgers.Unlock()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		delete(sessionLedgers.files, path)
		return 0, 0, 0, nil
	}
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return 0, 0, 0, err
	}
	c := sessionLedgers.files[path]
	if !c.reusable(info) {
		c = &sessionLedger{totals: make(map[string][3]int)}
		sessionLedgers.files[path] = c
	}
	totals, err := c.readTail(f, info.Size(), id)
	if err != nil {
		delete(sessionLedgers.files, path)
	}
	c.info = info
	return totals[0], totals[1], totals[2], err
}

func (c *sessionLedger) reusable(info os.FileInfo) bool {
	return c != nil && os.SameFile(c.info, info) && info.Size() >= c.info.Size() &&
		(info.Size() != c.info.Size() || info.ModTime().Equal(c.info.ModTime()))
}

func (c *sessionLedger) readTail(f *os.File, size int64, id string) ([3]int, error) {
	if c.offset == size {
		return c.totals[id], nil
	}
	var tail [3]int
	_, err := textutil.ScanRawLines(io.NewSectionReader(f, c.offset, size-c.offset), false, func(line []byte) {
		complete := line[len(line)-1] == '\n'
		if complete {
			c.offset += int64(len(line))
		}
		var e Entry
		if _, err := schemas.Decode(format, line, &e); err != nil || e.Session == "" {
			return
		}
		// An unterminated entry counts now but must be re-read if the next write extends it.
		if !complete {
			if e.Session == id {
				tail = [3]int{e.Input, e.Output, e.CacheRead}
			}
			return
		}
		sum := c.totals[e.Session]
		c.totals[e.Session] = [3]int{sum[0] + e.Input, sum[1] + e.Output, sum[2] + e.CacheRead}
	})
	sum := c.totals[id]
	return [3]int{sum[0] + tail[0], sum[1] + tail[1], sum[2] + tail[2]}, err
}

const schemaVersion = 1

var format = schemas.Register(schemas.Format{
	Name: "usage-ledger", Owner: "internal/tokenledger", Kind: schemas.Log,
	Glob:    Path,
	New:     func() any { return &Entry{} },
	Version: schemaVersion, Keep: "usage history is the ledger of record", Migrate: schemas.SameShape,
})
