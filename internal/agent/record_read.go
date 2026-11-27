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

// RunningRecent returns up to n running records newest first, using the active sidecar index when synced.
func RunningRecent(n int) ([]Record, error) {
	if n < 1 {
		return nil, nil
	}
	dir := paths.DispatchDir()
	if recs, ok := readActiveRecent(dir, n); ok {
		return recs, nil
	}
	recs, err := ReadRecentWith(AllRecords, inflight.RunningToken)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, r := range recs {
		if r.Status == "running" {
			out = append(out, r)
		}
	}
	seedActiveIndex(dir, out)
	if len(out) > n {
		out = out[:n]
	}
	return out, nil
}

const activeIndexedFile = ".indexed"

func indexedPath(dir string) string { return filepath.Join(activeDir(dir), activeIndexedFile) }

// activeIndexValid reports whether .indexed exists and is at least as new as dir itself.
func activeIndexValid(dir string) bool {
	sfi, err := os.Stat(indexedPath(dir))
	if err != nil {
		return false
	}
	dfi, err := os.Stat(dir)
	return err == nil && !dfi.ModTime().After(sfi.ModTime())
}

// activeIndexCurrent reports .indexed exists and still covers dir after a second dir stat.
func activeIndexCurrent(dir string) bool {
	if _, err := os.Stat(indexedPath(dir)); err != nil {
		return false
	}
	dfi, err := os.Stat(dir)
	if err != nil {
		return false
	}
	sfi, err := os.Stat(indexedPath(dir)) // after dir: a raw write between the stats must not look current
	return err == nil && !dfi.ModTime().After(sfi.ModTime())
}

// dropActiveIndexed removes the sentinel so readers fall back to a full scan instead of a false negative.
func dropActiveIndexed(dir string) { _ = os.Remove(indexedPath(dir)) }

// refreshActiveIndexed advances a current sentinel, or creates one only for a lone record.
func refreshActiveIndexed(dir string) {
	if activeIndexCurrent(dir) || loneRecord(dir) {
		touchActiveIndexed(dir)
		return
	}
	dropActiveIndexed(dir)
}

// loneRecord reports dir lists at most one dispatch record.
func loneRecord(dir string) bool {
	names, err := recordNames(dir)
	return err == nil && len(names) <= 1
}

// touchActiveIndexed stamps .indexed so its mtime matches or follows dir's mtime.
func touchActiveIndexed(dir string) {
	idx := indexedPath(dir)
	dfi, err := os.Stat(dir)
	if err != nil {
		_ = os.WriteFile(idx, nil, 0o600)
		return
	}
	now := time.Now()
	if dfi.ModTime().After(now) {
		now = dfi.ModTime()
	}
	if sfi, serr := os.Stat(idx); serr == nil && !dfi.ModTime().After(sfi.ModTime()) {
		return
	}
	if err := os.WriteFile(idx, nil, 0o600); err != nil || os.Chtimes(idx, now, now) != nil {
		_ = os.Remove(idx)
	}
}

// readActiveRecent reads up to n running records from activeDir(dir), unlinking stale markers lazily.
func readActiveRecent(dir string, n int) ([]Record, bool) {
	if !activeIndexValid(dir) {
		return nil, false
	}
	names, err := recordNames(activeDir(dir))
	if err != nil {
		return nil, false
	}
	var out []Record
	var buf []byte
	for _, name := range slices.Backward(names) {
		var b []byte
		var rerr error
		b, buf, rerr = readInto(filepath.Join(dir, name), buf)
		if rerr != nil {
			if errors.Is(rerr, fs.ErrNotExist) {
				_ = os.Remove(filepath.Join(activeDir(dir), name))
			}
			continue
		}
		r, derr := decodeRecord(b)
		if derr != nil {
			continue
		}
		if r.Status != "running" {
			_ = os.Remove(filepath.Join(activeDir(dir), name))
			continue
		}
		out = append(out, r)
		if len(out) >= n {
			break
		}
	}
	return out, true
}

// seedActiveIndex reconciles markers from a snapshot taken under the active-index lock.
func seedActiveIndex(dir string, _ []Record) {
	withActiveLock(dir, func() error {
		if os.MkdirAll(activeDir(dir), 0o700) != nil {
			dropActiveIndexed(dir)
			return nil
		}
		if !seedActiveLocked(dir, runningNow(dir)) {
			dropActiveIndexed(dir) // dir changed under the snapshot: keep markers, do not certify
		}
		return nil
	})
}

// activeRebuildHook runs after the locked snapshot and before stale markers are removed (tests interleave a claim).
var activeRebuildHook func(dir string)

// seedActiveLocked writes markers for running and drops the rest; false means dir changed under the snapshot.
func seedActiveLocked(dir string, running []Record) bool {
	before, err := os.Stat(dir)
	if err != nil {
		return false
	}
	gen := activeGen.Load()
	want, failed := markerWant(dir, running)
	if activeRebuildHook != nil {
		activeRebuildHook(dir)
	}
	if failed || !snapHeld(dir, before, gen) || !dropStaleMarkers(dir, want) || !snapHeld(dir, before, gen) {
		return false
	}
	touchActiveIndexed(dir)
	return snapHeld(dir, before, gen)
}

// snapHeld reports dir's mtime and the Save/claim generation are unchanged since the snapshot.
func snapHeld(dir string, before os.FileInfo, gen uint64) bool {
	if gen != activeGen.Load() {
		return false
	}
	after, err := os.Stat(dir)
	return err == nil && os.SameFile(before, after) && !after.ModTime().After(before.ModTime())
}

// markerWant writes one marker per running record; failed means a write did not land.
func markerWant(dir string, running []Record) (map[string]bool, bool) {
	want := make(map[string]bool, len(running))
	failed := false
	for _, r := range running {
		if r.Status != "running" || r.ID == "" {
			continue
		}
		name := r.ID + ".json"
		want[name] = true
		if err := os.WriteFile(filepath.Join(activeDir(dir), name), nil, 0o600); err != nil {
			failed = true
		}
	}
	return want, failed
}

// dropStaleMarkers removes active markers not in want; false means a removal failed and the index is unsafe.
func dropStaleMarkers(dir string, want map[string]bool) bool {
	names, err := recordNames(activeDir(dir))
	if err != nil {
		return false
	}
	for _, name := range names {
		if want[name] {
			continue
		}
		if err := os.Remove(filepath.Join(activeDir(dir), name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	}
	return true
}

// runningNow lists dir's running records; it runs under the active-index lock during a rebuild.
func runningNow(dir string) []Record {
	var running []Record
	_ = eachRecent(dir, inflight.RunningToken, func(r Record) bool {
		if r.Status == "running" {
			running = append(running, r)
		}
		return true
	})
	return running
}

// ReconcileActive syncs active markers with dir's running records under one lock.
func ReconcileActive(dir string) {
	if dir == "" {
		dir = paths.DispatchDir()
	}
	if _, err := os.Stat(dir); err != nil {
		return
	}
	seedActiveIndex(dir, nil)
}

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
	_ = os.Remove(filepath.Join(activeDir(dir), name))
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
