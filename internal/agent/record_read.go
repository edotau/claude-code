package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/parallel"
	"github.com/edotau/claude-code/internal/paths"
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

// eachRecent calls fn newest first on each decodable record in dir holding needle (jsonToken, RunningToken, "" = all).
func eachRecent(dir, needle string, fn func(Record) bool) error {
	names, err := recordNames(dir)
	probe := []byte(needle)
	var buf []byte // one read buffer for every record
	for _, n := range slices.Backward(names) {
		var rerr error
		var b []byte
		b, buf, rerr = readInto(filepath.Join(dir, n), buf)
		if rerr != nil || !bytes.Contains(b, probe) {
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
	got, _ := parallel.Map(context.Background(), names, 8, func(_ context.Context, n string) (*Record, error) {
		b, _, rerr := readInto(filepath.Join(dir, n), nil)
		if rerr != nil || !bytes.Contains(b, probe) {
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
