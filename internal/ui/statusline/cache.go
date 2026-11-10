package statusline

// Each render is a fresh process, so memos live in stamp files under the state dir ("f1|f2|…"); any
// failure falls open to a live recompute, never to a wrong bar.

import (
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

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
