package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
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
		if got := parseVersion(tc.in); got != tc.want {
			t.Errorf("parseVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// vendorRows mirror the codex, copilot and opencode register rows' version bounds; importing them would be a cycle.
var vendorRows = []agent.Schema{
	{Name: "codex", CLI: agent.CLI{Binary: "codex"}},
	{Name: "copilot", CLI: agent.CLI{Binary: "copilot", MinVersion: "1.0.104", MinReason: "named-provider model registry needs it"}},
	{Name: "opencode", CLI: agent.CLI{Binary: "opencode", MaxVersion: "1.x", MaxReason: "2.x broke the harness opencode route"}},
}

func TestVersionProblemUsesRegisteredRowBounds(t *testing.T) {
	for _, tc := range []struct {
		agent, version, wantPrefix string
	}{
		{"copilot", "1.0.103", "below 1.0.104: "},
		{"copilot", "1.0.104", ""},
		{"opencode", "1.9.9", ""},
		{"opencode", "2.0.0", "above 1.x: "},
		{"codex", "0.0.1", ""},
		{"codex", "99.0.0", ""},
	} {
		s, ok := agent.LookupSchema(tc.agent)
		if !ok {
			t.Fatalf("no registered row %q", tc.agent)
		}
		got := VersionProblem(s.CLI, tc.version)
		if !strings.HasPrefix(got, tc.wantPrefix) || (tc.wantPrefix == "") != (got == "") {
			t.Errorf("VersionProblem(%s, %s) = %q, want prefix %q", tc.agent, tc.version, got, tc.wantPrefix)
		}
	}
}

// probeFixture is a fake CLI that appends to a counter file per invocation and prints body.
func probeFixture(t *testing.T, body string) (bin, counter string) {
	hosttest.SkipOnWindows(t, "sh script")
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
		if v, err := versionAt(bin); err != nil || v != "3.2.1" {
			t.Fatalf("versionAt = %q, %v", v, err)
		}
	}
	if n := invocations(t, counter); n != 1 {
		t.Fatalf("two lookups ran --version %d times, want 1", n)
	}

	// A fresh process (empty memo) is served by the disk cache.
	resetMemo()
	if v, _ := versionAt(bin); v != "3.2.1" || invocations(t, counter) != 1 {
		t.Fatalf("disk cache missed: v=%q runs=%d", v, invocations(t, counter))
	}

	// A new mtime is a new build: probe again.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(bin, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := versionAt(bin); err != nil {
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
	if _, err := versionAt(bin); !errors.Is(err, errVersionUnknown) {
		t.Fatalf("failing probe err = %v, want errVersionUnknown", err)
	}
	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := versionAt(bin); err != nil || v != "4.0.0" {
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
	if v, err := versionAt(bin); err != nil || v != "5.0.0" {
		t.Fatalf("corrupt cache: %q, %v", v, err)
	}
	// The probe result replaced the corrupt file with a valid cache.
	resetMemo()
	if v, _ := versionAt(bin); v != "5.0.0" || invocations(t, counter) != 1 {
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
			key, ok := paths.BinKey(bin)
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
			if v, err := versionAt(bin); err != nil || v != "7.1.0" {
				t.Fatalf("versionAt = %q, %v (stale cache served?)", v, err)
			}
			if n := invocations(t, counter); n != 1 {
				t.Fatalf("probe ran %d times, want 1", n)
			}
			assertVersionCacheSchema1(t)
		})
	}
}

func assertVersionCacheSchema1(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(versionCacheFile())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil || string(doc["schema"]) != "1" {
		t.Fatalf("rewritten cache = %s, want schema 1", raw)
	}
}

func TestProbedVersionLineSharesCacheAndSurvivesConcurrentWrites(t *testing.T) {
	bin, counter := probeFixture(t, "echo 'fake 4.5.6'")
	resetMemo()
	var calls int
	probe := func(p string) string {
		calls++
		return FirstVersionLine(context.Background(), p, versionProbeTimeout)
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
	if v, err := versionAt(bin); err != nil || v != "4.5.6" || invocations(t, counter) != 1 {
		t.Fatalf("versionAt = %q, %v after %d runs; want a cache hit", v, err, invocations(t, counter))
	}

	assertConcurrentVersionWrites(t)
}

// assertConcurrentVersionWrites checks that concurrent writers do not drop each other's rows.
func assertConcurrentVersionWrites(t *testing.T) {
	t.Helper()
	const n = 24
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			p := "/fake/bin" + strconv.Itoa(i)
			writeVersionCache(p, p+"|1|1", "v"+strconv.Itoa(i))
		})
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

func killPidFileChild(t *testing.T, pidFile string) {
	t.Helper()
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// FirstVersionLine honours the caller's timeout: a hung --version is "" well before versionProbeTimeout.
func TestFirstVersionLineHonoursTimeout(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Cleanup(func() { killPidFileChild(t, pidFile) })
	bin, _ := probeFixture(t, "sleep 3 & echo $! > '"+pidFile+"'\nwait\necho 'fake 1.0.0'")
	// the script prints after 3s, so a timeout other than the caller's 100ms would yield "fake 1.0.0"
	if got := FirstVersionLine(t.Context(), bin, 100*time.Millisecond); got != "" {
		t.Fatalf("hung probe = %q, want \"\"", got)
	}
	fast, _ := probeFixture(t, "echo 'fake 1.0.0'\necho second")
	if got := FirstVersionLine(t.Context(), fast, time.Second); got != "fake 1.0.0" {
		t.Fatalf("FirstVersionLine = %q, want the first line", got)
	}
}

// TestSaltedRowsCoexistAndAChangedStampDropsBoth: salts of one build share the file; a new build evicts every old row.
func TestSaltedRowsCoexistAndAChangedStampDropsBoth(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	const p = "/fake/bin"
	writeVersionCache(p, p+"|1|1", "v")
	writeVersionCache(p, p+"|1|1|a", "A")
	writeVersionCache(p, p+"|1|1|b|c", "B")
	if c := readVersionCache(); c[p+"|1|1"] != "v" || c[p+"|1|1|a"] != "A" || c[p+"|1|1|b|c"] != "B" {
		t.Fatalf("salted rows erased each other: %v", c)
	}
	writeVersionCache(p, p+"|1|1|a", "A2")
	if c := readVersionCache(); len(c) != 3 || c[p+"|1|1|a"] != "A2" {
		t.Fatalf("same path+salt must replace its row only: %v", c)
	}
	writeVersionCache(p, p+"|2|2|a", "new")
	if c := readVersionCache(); len(c) != 1 || c[p+"|2|2|a"] != "new" {
		t.Fatalf("a changed stamp must drop every old row: %v", c)
	}
}

// TestProbedSaltedLineIsCachedAcrossProcesses: a fresh in-process memo reads the answer from the state file.
func TestProbedSaltedLineIsCachedAcrossProcesses(t *testing.T) {
	bin, _ := probeFixture(t, "echo 'fake 1.0.0'")
	resetMemo()
	var calls int
	probe := func(string) string { calls++; return "true" }
	for range 2 {
		if got := ProbedSaltedLine(bin, "exec-json", probe); got != "true" {
			t.Fatalf("line = %q", got)
		}
		resetMemo() // a second process starts with an empty memo
	}
	if got := ProbedSaltedLine(bin, "", func(string) string { return "ver" }); got != "ver" {
		t.Fatalf("the unsalted row = %q; want it kept apart from the salted one", got)
	}
	if got := ProbedSaltedLine(bin, "exec-json", probe); got != "true" || calls != 1 {
		t.Fatalf("salted row = %q after %d probes; want true after 1", got, calls)
	}
}

// TestDropSaltedLineRemovesOnlyItsRow: the dropped salt leaves the disk and memo; other salts of the build stay.
func TestDropSaltedLineRemovesOnlyItsRow(t *testing.T) {
	bin, _ := probeFixture(t, "echo 'fake 1.0.0'")
	resetMemo()
	ProbedSaltedLine(bin, "exec-json", func(string) string { return "false" })
	ProbedSaltedLine(bin, "other", func(string) string { return "keep" })
	DropSaltedLine(bin, "exec-json")
	resetMemo()
	var probed bool
	ProbedSaltedLine(bin, "exec-json", func(string) string { probed = true; return "" })
	if !probed {
		t.Fatal("the dropped row was still cached")
	}
	if got := ProbedSaltedLine(bin, "other", func(string) string { return "changed" }); got != "keep" {
		t.Fatalf("other salt = %q, want it kept", got)
	}
}
