package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/platform/parallel"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/routing/inflight"
)

// AllRecords is RecentRecords' n for "every record": the log itself is capped by the dispatch keep.
const AllRecords = 1 << 20

// RecentRecords returns up to n records, newest first; an unreadable dispatch dir lists none.
func RecentRecords(n int) []Record {
	out, _ := ReadRecent(n)
	return out
}

// ReadRecent is RecentRecords that also returns an unreadable dispatch dir's error.
func ReadRecent(n int) ([]Record, error) {
	recs, err := ReadRecentWith(n, "")
	for i, r := range recs {
		recs[i] = reapLost(r)
	}
	return recs, err
}

// RecordsSince is RecentRecords(AllRecords) without files last written before since (never opened); callers check Time.
func RecordsSince(since time.Time) []Record {
	recs, _ := readAllRecent(paths.DispatchDir(), "", since)
	for i, r := range recs {
		recs[i] = reapLost(r)
	}
	return recs
}

// ReadRecentWith is ReadRecent over the records whose file bytes hold needle (see eachRecent).
func ReadRecentWith(n int, needle string) ([]Record, error) {
	if n < 1 {
		return nil, nil
	}
	if n == AllRecords {
		return readAllRecent(paths.DispatchDir(), needle, time.Time{})
	}
	var out []Record
	err := eachRecent(paths.DispatchDir(), needle, func(r Record) bool {
		out = append(out, r)
		return len(out) < n
	})
	return out, err
}

// RunningRecent is ReadRecentWith over the records whose bytes mention a running status; callers still check Status.
func RunningRecent(n int) ([]Record, error) { return ReadRecentWith(n, inflight.RunningToken) }

// jsonToken is s as the record writer encodes it, the bytes any record holding s as a field value must contain.
func jsonToken(s string) string {
	if s == "" {
		return ""
	}
	b, _ := json.Marshal(s) // a string always marshals
	return string(b)
}

// settledScan is the not-running index for a RunningToken scan of dir; any other needle gets none (nil skips nothing).
func settledScan(dir, needle string) *inflight.Settled {
	if needle != inflight.RunningToken {
		return nil
	}
	return inflight.NewSettled(dir)
}

// eachRecent calls fn newest first on each decodable record in dir holding needle (jsonToken, RunningToken, "" = all).
func eachRecent(dir, needle string, fn func(Record) bool) error {
	names, err := recordNames(dir)
	probe := []byte(needle)
	settled := settledScan(dir, needle)
	defer settled.Done()
	var buf []byte // one read buffer for every record
	for _, n := range slices.Backward(names) {
		if settled.Skip(n) {
			continue
		}
		var rerr error
		var b []byte
		b, buf, rerr = readInto(filepath.Join(dir, n), buf)
		if rerr != nil || !bytes.Contains(b, probe) {
			if rerr == nil {
				settled.Note(n)
			}
			continue
		}
		if r, derr := decodeRecord(b); derr == nil && !fn(r) {
			break
		}
	}
	return err
}

// readAllRecent is eachRecent over every record last written at or after since, read concurrently, newest first.
func readAllRecent(dir, needle string, since time.Time) ([]Record, error) {
	names, err := recordNames(dir)
	if !since.IsZero() {
		// a record is written at or after its Time, so an older mtime rules it out without opening it
		names = slices.DeleteFunc(names, func(n string) bool {
			fi, serr := os.Lstat(filepath.Join(dir, n))
			return serr == nil && fi.ModTime().Before(since)
		})
	}
	return readNames(dir, names, needle), err
}

// readNames decodes the named records (oldest first; reversed in place) holding needle concurrently, newest first.
func readNames(dir string, names []string, needle string) []Record {
	slices.Reverse(names)
	probe := []byte(needle)
	settled := settledScan(dir, needle)
	defer settled.Done()
	got, _ := parallel.Map(context.Background(), names, 8, func(_ context.Context, n string) (*Record, error) {
		if settled.Skip(n) {
			return nil, nil
		}
		b, _, rerr := readInto(filepath.Join(dir, n), nil)
		if rerr != nil || !bytes.Contains(b, probe) {
			if rerr == nil {
				settled.Note(n)
			}
			return nil, nil
		}
		if r, derr := decodeRecord(b); derr == nil {
			return &r, nil
		}
		return nil, nil
	})
	out := make([]Record, 0, len(got))
	for _, r := range got {
		if r != nil {
			out = append(out, *r)
		}
	}
	return out
}

var errNotRegular = errors.New("not a regular file")

// readInto reads regular file path into buf's storage (grown as needed); returns the bytes and the buffer to reuse.
func readInto(path string, buf []byte) ([]byte, []byte, error) {
	// O_NONBLOCK: opening a FIFO with no writer returns at once instead of hanging the reader
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, buf, err
	}
	defer f.Close()
	if fi, serr := f.Stat(); serr != nil || !fi.Mode().IsRegular() {
		return nil, buf, errors.Join(errNotRegular, serr)
	}
	buf = buf[:0]
	for {
		if len(buf) == cap(buf) {
			buf = slices.Grow(buf, 4096)
		}
		n, err := f.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			return buf, buf, nil
		}
		if err != nil {
			return nil, buf, err
		}
	}
}

// ErrNoRecord means no dispatch record matches the id.
var ErrNoRecord = errors.New("0 matches")

// noMatch is the error for n prefix matches of id; zero wraps ErrNoRecord.
func noMatch(id string, names []string, n int) error {
	why := ErrNoRecord
	if n > 0 {
		why = fmt.Errorf("%d matches", n)
	}
	return fmt.Errorf("dispatch %q: %w (see claude-code agents status); nearest: %s",
		id, why, strings.Join(nearestIDs(names, id), ", "))
}

// pruneDispatches trims the oldest records past dispatchKeep, never the ids in keep nor an active record.
func pruneDispatches(dir string, keep ...string) {
	names, err := recordNames(dir)
	if err != nil || len(names) <= dispatchKeep+dispatchPruneMargin { // best-effort: a failed listing skips this prune
		return
	}
	excess := len(names) - dispatchKeep
	for _, name := range names {
		if excess <= 0 {
			return
		}
		if slices.Contains(keep, strings.TrimSuffix(name, ".json")) || activeRecord(filepath.Join(dir, name)) {
			continue
		}
		if !removeRecord(dir, name) {
			continue
		}
		excess--
	}
}

// removeRecord deletes one dispatch record and its files under its lock; false means it was held or revived.
func removeRecord(dir, name string) bool {
	removed := false
	// best-effort: a lock that cannot be opened skips this record, as a held one does
	ran, _ := paths.TryFileLock(filepath.Join(dir, name+".lock"), 0, func() error {
		if activeRecord(filepath.Join(dir, name)) { // a claim or update may land between the caller's check and the lock
			return nil
		}
		removeRecordFiles(dir, name)
		removed = true
		return nil
	})
	return ran && removed
}

// removeRecordFiles deletes the record's files; its .lock goes last, while the caller still holds it.
func removeRecordFiles(dir, name string) {
	id := strings.TrimSuffix(name, ".json")
	_ = os.Remove(filepath.Join(dir, name))
	_ = os.Remove(filepath.Join(dir, id+".log"))
	_ = os.Remove(filepath.Join(dir, id+paths.DispatchEventsSuffix))
	_ = os.Remove(AgyLogPath(id))
	_ = os.Remove(proxy.RanIdentityPath(id))
	_ = os.Remove(filepath.Join(dir, id+wire.LiveSockSuffix))
	if marker := WatchMarker("resumed", id); marker != "" {
		_ = os.Remove(marker) // without the record's cleared claim, a rollback's leftover marker would revive the adoption
	}
	_ = os.Remove(filepath.Join(dir, name+".lock"))
}

// RecordLive reports whether rec's job is still running, reloading its record so a resumed PID counts.
func RecordLive(rec Record) bool {
	if r, err := LoadRecord(rec.ID); err == nil { // a pruned record falls back to the one given
		rec = r
	}
	if rec.PID <= 0 || !proc.Alive(rec.PID) {
		return false
	}
	got, err := proc.StartTime(rec.PID)
	return rec.PIDStart == "" || err != nil || got == rec.PIDStart // another start time is a reused pid
}

// LiveJobIn is the first running job outside the caller's ancestry (skip) whose tree is inside lane and is alive.
func LiveJobIn(lane string, skip map[int]bool) (Record, bool, error) {
	recs, err := RunningRecent(AllRecords)
	for _, r := range recs {
		if r.Status == "running" && r.Tree != "" && !skip[r.PID] && paths.InsideDir(r.Tree, lane) && jobLive(r) {
			return r, true, nil
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		err = nil // no dispatch dir yet: no jobs
	}
	return Record{}, false, err
}

// jobLive is RecordLive, except a pid-less record counts as live for its first minute.
func jobLive(r Record) bool {
	if r.PID <= 0 {
		return r.Alive()
	}
	return RecordLive(r)
}
