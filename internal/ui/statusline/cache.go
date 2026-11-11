package statusline

import (
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/inflight"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/tokenledger"
)

// Each render is a fresh process, so memos live in stamp files under the state dir ("f1|f2|…"); any
// failure falls open to a live recompute, never to a wrong bar.

func stampFile(kind, key string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return filepath.Join(paths.StateDir(), "statusline", kind+"-"+strconv.FormatUint(h.Sum64(), 16))
}

func readStamp(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "|")
}

func writeStamp(path string, fields ...string) {
	if os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		_ = paths.AtomicWrite(path, []byte(strings.Join(fields, "|")), 0o600)
	}
}

// freshStamp returns the fields after a leading unix-ms timestamp younger than ttl, else nil.
func freshStamp(path string, ttl time.Duration, n int) []string {
	f := readStamp(path)
	if len(f) != n+1 {
		return nil
	}
	ts, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || time.Since(time.UnixMilli(ts)) >= ttl {
		return nil
	}
	return f[1:]
}

func writeTimedStamp(path string, fields ...string) {
	writeStamp(path, append([]string{strconv.FormatInt(time.Now().UnixMilli(), 10)}, fields...)...)
}

// ledgerTotals memos (ledger size, mtime) → this session's dispatch tokens: the ledger only changes by appending.
func ledgerTotals(session string) (in, out int) {
	if session == "" {
		return 0, 0
	}
	fi, statErr := os.Stat(tokenledger.Path())
	stamp := stampFile("ledger", paths.StateDir()+"\x00"+session)
	size, mtime := "", ""
	if statErr == nil {
		size, mtime = strconv.FormatInt(fi.Size(), 10), strconv.FormatInt(fi.ModTime().UnixNano(), 10)
		if f := readStamp(stamp); len(f) == 4 && f[0] == size && f[1] == mtime {
			i, ierr := strconv.Atoi(f[2])
			o, oerr := strconv.Atoi(f[3])
			if ierr == nil && oerr == nil {
				return i, o
			}
		}
	}
	in, out, _ = tokenledger.TotalsBySession(session)
	if statErr == nil {
		writeStamp(stamp, size, mtime, strconv.Itoa(in), strconv.Itoa(out))
	}
	return in, out
}

// runningTTL caps the marker + dispatch sweep at one per 3s across renders.
const runningTTL = 3 * time.Second

// runningDispatches is the live dispatch count for this session, cached for runningTTL.
func runningDispatches(in Input) int {
	stamp := stampFile("running", paths.StateDir()+"\x00"+in.TranscriptPath)
	if f := freshStamp(stamp, runningTTL, 1); f != nil {
		n, _ := strconv.Atoi(f[0])
		return n
	}
	n := inflight.LiveDispatches(inflight.SessionOwner())
	writeTimedStamp(stamp, strconv.Itoa(n))
	return n
}
