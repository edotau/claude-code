package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/schemas"
)

// Named failures of AgentVersion; callers match with errors.Is.
var (
	ErrAgentNotInstalled = errors.New("not installed")
	ErrVersionUnknown    = errors.New("version not recognized")
)

// versionRe picks the first dotted version in a --version line, never digits glued to a name ("gpt-5.1" stays a name).
var versionRe = regexp.MustCompile(`(?:^|[^0-9A-Za-z.\-])v?(\d+(?:\.\d+)+)`)

// ParseVersion returns the first dotted version in a CLI's --version output ("codex-cli 0.155.0" -> "0.155.0"), else "".
func ParseVersion(line string) string {
	if m := versionRe.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

// AgentVersion is the installed version of the named agent's CLI, resolved as a launch resolves it (bin/ link, then PATH).
func AgentVersion(name string) (string, error) {
	spec, ok := providers.LookupAgentSpec(name)
	if !ok {
		return "", fmt.Errorf("unknown agent %q", name)
	}
	path, err := paths.VendorBin(name)
	if err != nil {
		bin := spec.Binary
		if bin == "" {
			bin = name // claude has no Binary on its row
		}
		if path, err = paths.LookPathReal(bin); err != nil {
			return "", fmt.Errorf("%w: %v", ErrAgentNotInstalled, err)
		}
	}
	return AgentVersionAt(path)
}

// AgentVersionAt is AgentVersion for a resolved binary path; --version runs once per path+size+mtime.
func AgentVersionAt(path string) (string, error) {
	line := versionLine(path)
	if v := ParseVersion(line); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%w: %q", ErrVersionUnknown, line)
}

// VersionProblem is "" when version sits inside the row's bounds, else "below <min>: <reason>" or "above <max>: <reason>".
func VersionProblem(spec providers.AgentSpec, version string) string {
	v := versionParts(version)
	if v == nil {
		return ""
	}
	if lo := versionParts(spec.MinVersion); lo != nil && compareParts(v, lo, false) < 0 {
		return "below " + spec.MinVersion + ": " + spec.MinReason
	}
	if hi, wild := boundParts(spec.MaxVersion); hi != nil && compareParts(v, hi, wild) > 0 {
		return "above " + spec.MaxVersion + ": " + spec.MaxReason
	}
	return ""
}

// VersionWarning is the one-line launch warning for the agent's binary at path, "" when in bounds or unknown.
func VersionWarning(name, path string) string {
	spec, ok := providers.LookupAgentSpec(name)
	if !ok || (spec.MinVersion == "" && spec.MaxVersion == "") {
		return "" // no bound: skip the probe entirely
	}
	v, err := AgentVersionAt(path)
	if err != nil {
		return ""
	}
	if p := VersionProblem(spec, v); p != "" {
		return fmt.Sprintf("claude-code: %s %s %s", name, v, p)
	}
	return ""
}

// versionParts splits "1.0.104" into ints; nil when s is empty or not dotted numbers.
func versionParts(s string) []int {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return nil
	}
	var out []int
	for p := range strings.SplitSeq(s, ".") {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// boundParts parses a bound, where a trailing ".x" or ".*" ("1.x") fixes only the leading parts; wild reports it.
func boundParts(s string) (parts []int, wild bool) {
	for _, suffix := range []string{".x", ".*"} {
		if t, ok := strings.CutSuffix(s, suffix); ok {
			return versionParts(t), true
		}
	}
	return versionParts(s), false
}

// compareParts orders a against b (<0, 0, >0), zero-padding the shorter; wild compares only b's leading parts.
func compareParts(a, b []int, wild bool) int {
	n := max(len(a), len(b))
	if wild {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

var (
	versionMu   sync.Mutex
	versionMemo = map[string]string{}
	// versionWriteMu serializes writeVersionCache's read-modify-write so concurrent callers cannot drop each other's rows.
	versionWriteMu sync.Mutex
)

func versionCacheFile() string { return filepath.Join(paths.StateDir(), "agentversion.json") }

// versionKey identifies one binary build; ok is false when path cannot be stat'ed.
func versionKey(path string) (key string, ok bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%s|%d|%d", path, fi.Size(), fi.ModTime().UnixNano()), true
}

// versionLine is firstVersionLine cached in-process and in the state dir, keyed by path+size+mtime; failures are not cached.
func versionLine(path string) string { return ProbedVersionLine(path, firstVersionLine) }

// ProbedVersionLine is versionLine with the caller's own probe (its timeout and parsing), sharing the same cache.
func ProbedVersionLine(path string, probe func(string) string) string {
	key, ok := versionKey(path)
	if !ok {
		return probe(path)
	}
	versionMu.Lock()
	line, hit := versionMemo[key]
	versionMu.Unlock()
	if hit {
		return line
	}
	if line = readVersionCache()[key]; line == "" {
		if line = probe(path); line == "" {
			return ""
		}
		writeVersionCache(path, key, line)
	}
	versionMu.Lock()
	versionMemo[key] = line
	versionMu.Unlock()
	return line
}

// readVersionCache loads the disk cache; a missing or corrupt file is an empty cache.
func readVersionCache() map[string]string {
	data, err := os.ReadFile(versionCacheFile())
	if err != nil {
		return map[string]string{}
	}
	var f versionFile // a v0 (bare map), newer or corrupt cache is a miss, never migrated
	if from, err := schemas.Decode(versionFormat, data, &f); err != nil || from != schemaVersion || f.Versions == nil {
		return map[string]string{}
	}
	return f.Versions
}

// writeVersionCache records key (dropping older builds of path) via temp file + rename; any failure is silent.
func writeVersionCache(path, key, line string) {
	versionWriteMu.Lock()
	defer versionWriteMu.Unlock()
	cache := readVersionCache()
	for k := range cache {
		if strings.HasPrefix(k, path+"|") {
			delete(cache, k)
		}
	}
	cache[key] = line
	data, err := json.Marshal(versionFile{Schema: schemaVersion, Versions: cache})
	if err != nil {
		return
	}
	file := versionCacheFile()
	if os.MkdirAll(filepath.Dir(file), 0o755) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".agentversion-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr != nil || cerr != nil || os.Rename(tmp.Name(), file) != nil {
		_ = os.Remove(tmp.Name())
	}
}

// schemaVersion is the agentversion cache's current schema version.
const schemaVersion = 1

// versionFile is agentversion.json on disk.
type versionFile struct {
	Schema   int               `json:"schema"`
	Versions map[string]string `json:"versions"`
}

var versionFormat = schemas.Register(schemas.Format{
	Name: "agentversion", Owner: "internal/agent", Kind: schemas.Cache,
	Glob:    versionCacheFile,
	New:     func() any { return &versionFile{} },
	Version: schemaVersion, Keep: "cache; a mismatch is a miss, rebuilt by the next --version probe",
})
