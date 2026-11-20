package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/edotau/claude-code/internal/inflight"
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

// ReadRecentWith is ReadRecent over the records whose file bytes hold needle (see eachRecent).
func ReadRecentWith(n int, needle string) ([]Record, error) {
	if n < 1 {
		return nil, nil
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

// readInto reads path into buf's storage (grown as needed) and returns the bytes and the buffer to reuse.
func readInto(path string, buf []byte) ([]byte, []byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, buf, err
	}
	defer f.Close()
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
