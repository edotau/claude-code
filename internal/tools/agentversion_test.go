package tools

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
)

func TestParseVersion(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"1.0.104", "1.0.104"},
		{"codex-cli 0.155.0", "0.155.0"},
		{"opencode 1.2.3", "1.2.3"},
		{"agy version 1.4.9 (abc)", "1.4.9"},
		{"v2.0.32", "2.0.32"},
		{"1.0.104-beta.1", "1.0.104"},
		{"GitHub Copilot CLI 1.0.104.", "1.0.104"},
		{"gpt-5.1 something 0.9.0", "0.9.0"},
		{"gpt-5.1", ""},
		{"build 7", ""},
		{"no version here", ""},
		{"", ""},
	} {
		if got := ParseVersion(tc.in); got != tc.want {
			t.Errorf("ParseVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestVersionProblem(t *testing.T) {
	spec := func(name string) providers.AgentSpec {
		s, ok := providers.LookupAgentSpec(name)
		if !ok {
			t.Fatalf("no spec %q", name)
		}
		return s
	}
	for _, tc := range []struct {
		agent, version, wantPrefix string
	}{
		{"copilot", "1.0.103", "below 1.0.104: "},
		{"copilot", "1.0.104", ""},
		{"copilot", "1.1.0", ""},
		{"opencode", "1.9.9", ""},
		{"opencode", "2.0.0", "above 1.x: "},
		{"codex", "0.0.1", ""},
		{"codex", "99.0.0", ""},
	} {
		got := VersionProblem(spec(tc.agent), tc.version)
		if tc.wantPrefix == "" && got != "" || !strings.HasPrefix(got, tc.wantPrefix) {
			t.Errorf("VersionProblem(%s, %s) = %q, want prefix %q", tc.agent, tc.version, got, tc.wantPrefix)
		}
		if tc.wantPrefix != "" && strings.TrimPrefix(got, tc.wantPrefix) == "" {
			t.Errorf("VersionProblem(%s, %s) = %q carries no reason", tc.agent, tc.version, got)
		}
	}
}

// probeFixture is a fake CLI that appends to a counter file per invocation and prints body.
func probeFixture(t *testing.T, body string) (bin, counter string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := t.TempDir()
	counter = filepath.Join(dir, "count")
	bin = filepath.Join(dir, "fake")
	script := "#!/bin/sh\necho x >> '" + counter + "'\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, counter
}

func invocations(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func resetMemo() {
	versionMu.Lock()
	versionMemo = map[string]string{}
	versionMu.Unlock()
}

func TestAgentVersionAtCache(t *testing.T) {
	bin, counter := probeFixture(t, "echo 'fake 3.2.1'")
	resetMemo()
	for range 2 {
		if v, err := AgentVersionAt(bin); err != nil || v != "3.2.1" {
			t.Fatalf("AgentVersionAt = %q, %v", v, err)
		}
	}
	if n := invocations(t, counter); n != 1 {
		t.Fatalf("two lookups ran --version %d times, want 1", n)
	}

	// A fresh process (empty memo) is served by the disk cache.
	resetMemo()
	if v, _ := AgentVersionAt(bin); v != "3.2.1" || invocations(t, counter) != 1 {
		t.Fatalf("disk cache missed: v=%q runs=%d", v, invocations(t, counter))
	}

	// A new mtime is a new build: probe again.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(bin, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := AgentVersionAt(bin); err != nil {
		t.Fatal(err)
	}
	if n := invocations(t, counter); n != 2 {
		t.Fatalf("after touch ran %d times, want 2", n)
	}
}

func TestAgentVersionAtFailureNotCached(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "ok")
	bin, counter := probeFixture(t, "[ -e '"+flag+"' ] || exit 1\necho 'fake 4.0.0'")
	resetMemo()
	if _, err := AgentVersionAt(bin); !errors.Is(err, ErrVersionUnknown) {
		t.Fatalf("failing probe err = %v, want ErrVersionUnknown", err)
	}
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := AgentVersionAt(bin); err != nil || v != "4.0.0" {
		t.Fatalf("recovered probe = %q, %v (failure was cached?)", v, err)
	}
	if n := invocations(t, counter); n != 2 {
		t.Fatalf("ran %d times, want 2", n)
	}
}

func TestAgentVersionAtCorruptCacheTolerated(t *testing.T) {
	bin, counter := probeFixture(t, "echo 'fake 5.0.0'")
	resetMemo()
	if err := os.MkdirAll(filepath.Dir(versionCacheFile()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(versionCacheFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := AgentVersionAt(bin); err != nil || v != "5.0.0" {
		t.Fatalf("corrupt cache: %q, %v", v, err)
	}
	// The probe result replaced the corrupt file with a valid cache.
	resetMemo()
	if v, _ := AgentVersionAt(bin); v != "5.0.0" || invocations(t, counter) != 1 {
		t.Fatalf("cache not rewritten: v=%q runs=%s", v, strconv.Itoa(invocations(t, counter)))
	}
}

// An old-shape (bare map) or schema-2 agentversion cache is a miss, and the re-probe writes "schema":1.
func TestAgentVersionCacheShapeMismatchIsMissThenWritesSchema1(t *testing.T) {
	for name, body := range map[string]string{
		"v0 bare map": `{"%KEY%":"fake 6.0.0"}`,
		"schema 2":    `{"schema":2,"versions":{"%KEY%":"fake 6.0.0"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			bin, counter := probeFixture(t, "echo 'fake 7.1.0'")
			resetMemo()
			key, ok := versionKey(bin)
			if !ok {
				t.Fatal("no key")
			}
			if err := os.MkdirAll(strings.TrimSuffix(versionCacheFile(), "/agentversion.json"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(versionCacheFile(), []byte(strings.ReplaceAll(body, "%KEY%", key)), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := readVersionCache(); len(got) != 0 {
				t.Fatalf("readVersionCache = %v, want empty (miss)", got)
			}
			if v, err := AgentVersionAt(bin); err != nil || v != "7.1.0" {
				t.Fatalf("AgentVersionAt = %q, %v (stale cache served?)", v, err)
			}
			if n := invocations(t, counter); n != 1 {
				t.Fatalf("probe ran %d times, want 1", n)
			}
			raw, err := os.ReadFile(versionCacheFile())
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]json.RawMessage
			if json.Unmarshal(raw, &doc) != nil || string(doc["schema"]) != "1" {
				t.Fatalf("rewritten cache = %s, want schema 1", raw)
			}
		})
	}
}

func TestProbedVersionLineSharesCacheAndSurvivesConcurrentWrites(t *testing.T) {
	bin, counter := probeFixture(t, "echo 'fake 4.5.6'")
	resetMemo()
	var calls int
	probe := func(p string) string {
		calls++
		return firstVersionLine(p)
	}
	for range 2 {
		if got := ProbedVersionLine(bin, probe); got != "fake 4.5.6" {
			t.Fatalf("ProbedVersionLine = %q", got)
		}
	}
	if calls != 1 || invocations(t, counter) != 1 {
		t.Fatalf("probe ran %d times, binary %d times; want 1, 1", calls, invocations(t, counter))
	}
	// The default path reads the same cache entry.
	if v, err := AgentVersionAt(bin); err != nil || v != "4.5.6" || invocations(t, counter) != 1 {
		t.Fatalf("AgentVersionAt = %q, %v after %d runs; want a cache hit", v, err, invocations(t, counter))
	}

	// Concurrent writers must not drop each other's rows.
	const n = 24
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := "/fake/bin" + strconv.Itoa(i)
			writeVersionCache(p, p+"|1|1", "v"+strconv.Itoa(i))
		}()
	}
	wg.Wait()
	cache := readVersionCache()
	for i := range n {
		p := "/fake/bin" + strconv.Itoa(i)
		if cache[p+"|1|1"] != "v"+strconv.Itoa(i) {
			t.Errorf("row %s missing after concurrent writes (cache has %d rows)", p, len(cache))
		}
	}
}
