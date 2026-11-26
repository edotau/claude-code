package statusline

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/dev/tokenledger"
	"github.com/edotau/claude-code/internal/dev/usage"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/routing/inflight"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// Each render is a fresh process, so memos live in stamp files; any failure falls open to a live recompute.

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
	if os.MkdirAll(filepath.Dir(path), 0o700) == nil { // Disposable stamps need atomic visibility, not fsync durability.
		_ = paths.AtomicWriteNoSync(path, []byte(strings.Join(fields, "|")), 0o600)
	}
}

// freshStamp returns the fields after a leading unix-ms timestamp younger than ttl, else nil.
func freshStamp(path string, ttl time.Duration, n int) []string {
	f := readStamp(path)
	if len(f) != n+1 {
		return nil
	}
	ts, err := strconv.ParseInt(f[0], 10, 64)
	if age := time.Since(time.UnixMilli(ts)); err != nil || age < 0 || age >= ttl { // a future stamp is corrupt
		return nil
	}
	return f[1:]
}

func writeTimedStamp(path string, fields ...string) {
	writeStamp(path, append([]string{strconv.FormatInt(time.Now().UnixMilli(), 10)}, fields...)...)
}

// fileMemo returns the n fields stamped for path's current (size, mtime) and a save that stamps fresh ones.
func fileMemo(stamp, path string, n int) (cached []string, save func(fields ...string)) {
	fi, err := os.Stat(path)
	if err != nil { // no key to validate against: never read or write the memo
		return nil, func(...string) {}
	}
	size, mtime := strconv.FormatInt(fi.Size(), 10), strconv.FormatInt(fi.ModTime().UnixNano(), 10)
	if f := readStamp(stamp); len(f) == n+2 && f[0] == size && f[1] == mtime {
		cached = f[2:]
	}
	return cached, func(fields ...string) { writeStamp(stamp, append([]string{size, mtime}, fields...)...) }
}

// ledgerTotals memos (ledger size, mtime) → this session's dispatch tokens: the ledger only changes by appending.
func ledgerTotals(session string) (in, out int) {
	if session == "" {
		return 0, 0
	}
	cached, save := fileMemo(stampFile("ledger", paths.StateDir()+"\x00"+session), tokenledger.Path(), 2)
	if cached != nil {
		i, ierr := strconv.Atoi(cached[0])
		o, oerr := strconv.Atoi(cached[1])
		if ierr == nil && oerr == nil {
			return i, o
		}
	}
	in, out, _, err := tokenledger.SessionTotals(session)
	if err == nil { // a failed read is partial: show it, never cache it
		save(strconv.Itoa(in), strconv.Itoa(out))
	}
	return in, out
}

// runningTTL caps the marker + dispatch sweep at one per 3s across renders.
const runningTTL = 3 * time.Second

// runningDispatches is the live dispatch count for this session, cached for runningTTL.
func runningDispatches(in input) int {
	stamp := stampFile("running", paths.StateDir()+"\x00"+in.TranscriptPath)
	if f := freshStamp(stamp, runningTTL, 1); f != nil {
		n, _ := strconv.Atoi(f[0])
		return n
	}
	n := inflight.LiveDispatches(inflight.SessionOwner())
	writeTimedStamp(stamp, strconv.Itoa(n))
	return n
}

// limitsCacheMaxAge bounds how stale internal/usage's oauth-usage cache may be before it is ignored.
const limitsCacheMaxAge = 15 * time.Minute

// limitsSegment is "7d:N%" on the first-party subscription session only; no fresh data or a passed reset omits it.
func limitsSegment(in input) string {
	week := limitsFromPayload(in)
	if week < 0 {
		week = limitsFromCache()
	}
	if week < 0 {
		return ""
	}
	return fmt.Sprintf("7d:%d%%", week)
}

// onSubscription is true when the session profile row is first-party Anthropic on its cli (passthrough) login.
func onSubscription(session string) bool {
	reg := registry()
	if reg == nil {
		return false
	}
	p := reg.Providers[session]
	return p != nil && p.Provider == "anthropic" && p.Auth.Method() == providers.AuthCLI
}

// pct is the window's rounded used percent; -1 when absent or its reset already passed.
func (w *rateWindow) pct(now int64) int {
	if w == nil || w.UsedPercentage == nil || (w.ResetsAt != 0 && w.ResetsAt <= now) {
		return -1
	}
	return clampPct(int(*w.UsedPercentage + 0.5))
}

// limitsFromPayload reads the 7d window off the statusline payload; -1 means absent or expired.
func limitsFromPayload(in input) int {
	if in.RateLimits == nil {
		return -1
	}
	return in.RateLimits.SevenDay.pct(time.Now().Unix())
}

// limitsFromCache returns internal/usage's cached 7d window without network; -1 if absent, stale, expired or foreign.
func limitsFromCache() int {
	util, resets, ok := usage.CachedSevenDay(limitsCacheMaxAge)
	if !ok {
		return -1
	}
	if t, err := time.Parse(time.RFC3339, resets); err != nil || !t.After(time.Now()) {
		return -1
	}
	return clampPct(int(util + 0.5))
}
