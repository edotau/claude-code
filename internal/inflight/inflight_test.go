package inflight

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
)

func TestSessionOwner(t *testing.T) {
	originalGetppid, originalComm, originalParent := sessionGetppid, sessionComm, sessionParent
	t.Cleanup(func() {
		sessionGetppid, sessionComm, sessionParent = originalGetppid, originalComm, originalParent
	})
	for _, tt := range []struct {
		name   string
		parent int
		shells int
		end    int
		want   int
	}{
		{name: "reparented to init", parent: 1},
		{name: "shell chain ending at init", parent: 20, shells: 2, end: 1},
		{name: "shell chain exceeds limit", parent: 20, shells: 9, end: 42},
		{name: "unknown parent", parent: 0},
		{name: "negative parent", parent: -1},
		{name: "unreadable shell parent", parent: 20, shells: 1},
		{name: "session process", parent: 42, want: 42},
		{name: "session behind shells", parent: 20, shells: 2, end: 42, want: 42},
		{name: "session at limit", parent: 20, shells: 8, end: 42, want: 42},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sessionGetppid = func() int { return tt.parent }
			sessionComm = func(pid int) string {
				if pid >= 20 && pid < 20+tt.shells {
					return "sh"
				}
				return "claude"
			}
			sessionParent = func(pid int) int {
				if pid == 20+tt.shells-1 {
					return tt.end
				}
				return pid + 1
			}
			if got := SessionOwner(); got != tt.want {
				t.Errorf("SessionOwner() = %d, want %d", got, tt.want)
			}
		})
	}
}

// fabricate stubs the ancestry: names[i] is the process at pid 100+i, whose parent is the next one up.
func fabricate(t *testing.T, names []string, end int) {
	t.Helper()
	g, c, p := sessionGetppid, sessionComm, sessionParent
	t.Cleanup(func() { sessionGetppid, sessionComm, sessionParent = g, c, p })
	sessionGetppid = func() int { return 100 }
	sessionComm = func(pid int) string {
		if pid < 100 || pid-100 >= len(names) {
			return ""
		}
		return names[pid-100]
	}
	sessionParent = func(pid int) int {
		if pid-100 == len(names)-1 {
			return end
		}
		return pid + 1
	}
}

func TestSessionOwnerThroughWrappers(t *testing.T) {
	for _, tt := range []struct {
		name  string
		chain []string
		end   int
		want  int
	}{
		{"direct", []string{"claude"}, 1, 100},
		{"rtk", []string{"rtk", "claude"}, 1, 101},
		{"timeout", []string{"timeout", "claude"}, 1, 101},
		{"env", []string{"env", "claude"}, 1, 101},
		{"sudo", []string{"sudo", "claude"}, 1, 101},
		{"nohup", []string{"nohup", "claude"}, 1, 101},
		{"time", []string{"time", "claude"}, 1, 101},
		{"nice", []string{"nice", "claude"}, 1, 101},
		{"stdbuf", []string{"stdbuf", "claude"}, 1, 101},
		{"xargs", []string{"xargs", "claude"}, 1, 101},
		{"make", []string{"make", "bash", "claude"}, 1, 102},
		{"go run", []string{"go", "zsh", "claude"}, 1, 102},
		{"dashed zsh", []string{"-zsh", "claude"}, 1, 101},
		{"dashed bash", []string{"-bash", "claude"}, 1, 101},
		{"fish", []string{"fish", "claude"}, 1, 101},
		{"ksh", []string{"ksh", "claude"}, 1, 101},
		{"stacked", []string{"rtk", "timeout", "sh", "claude"}, 1, 103},
		{"node cli", []string{"rtk", "node"}, 1, 101},
		{"no agent cli falls back", []string{"rtk", "sh", "tmux"}, 1, 102},
		{"wrapper is top", []string{"env", "rtk"}, 1, 0},
		{"wrapper top with live parent", []string{"env"}, 50, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fabricate(t, tt.chain, tt.end)
			if got := SessionOwner(); got != tt.want {
				t.Errorf("SessionOwner() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSessionOwnerWrapperBound(t *testing.T) {
	chain := slices.Repeat([]string{"rtk"}, 9)
	fabricate(t, append(chain, "claude"), 1)
	if got := SessionOwner(); got != 0 {
		t.Errorf("9 wrappers: SessionOwner() = %d, want 0 (bound)", got)
	}
	fabricate(t, append(chain[:8:8], "claude"), 1)
	if got := SessionOwner(); got != 108 {
		t.Errorf("8 wrappers: SessionOwner() = %d, want 108", got)
	}
}

func BenchmarkSessionOwner(b *testing.B) {
	g, c, p := sessionGetppid, sessionComm, sessionParent
	b.Cleanup(func() { sessionGetppid, sessionComm, sessionParent = g, c, p })
	names := []string{"rtk", "timeout", "-zsh", "claude"}
	sessionGetppid = func() int { return 100 }
	sessionComm = func(pid int) string { return names[pid-100] }
	sessionParent = func(pid int) int { return pid + 1 }
	for b.Loop() {
		SessionOwner()
	}
}

func TestLiveRoutesObjectAndLegacyString(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	dir := paths.DispatchDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	now := time.Now().UTC().Format(time.RFC3339)
	recs := map[string]string{
		"a.json": `"route":{"provider":"databricks","profile":"dev"}`,
		"b.json": `"route":"databricks:qa"`,
		"c.json": `"route":null`,
	}
	for name, route := range recs {
		body := fmt.Sprintf(`{"status":"running","pid":%d,"time":%q,%s}`, pid, now, route)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := LiveRoutes()
	if len(got) != 2 || got["databricks:dev"] != 1 || got["databricks:qa"] != 1 {
		t.Fatalf("LiveRoutes = %v, want databricks:dev and databricks:qa once each", got)
	}
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }

func benchmarkDispatchRecords(b *testing.B) {
	b.Helper()
	if !probeTrusted() {
		b.Skip("live dispatch records require the production Unix PID probe")
	}
	b.Setenv("CLAUDE_CONFIG_DIR", b.TempDir())
	dir := paths.DispatchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		b.Fatal(err)
	}
	// Model multi-kilobyte dispatch briefs without importing agent, which would create an import cycle.
	brief := strings.Repeat("Review the dispatch lifecycle for correctness, allocations, and error handling. ", 160)
	for i := range 2000 {
		id := fmt.Sprintf("20261122T120000-%08d", i)
		status := "ok"
		if i%400 == 0 {
			status = "running"
		}
		route := []string{"databricks:qa", "databricks:dev"}[(i/400)%2]
		request := map[string]any{"Prompt": "", "Model": "sonnet", "Provider": "databricks", "Tools": true}
		record := map[string]any{
			"schema": 1, "id": id, "time": "2026-11-22T12:00:00Z", "agent": "claude", "request": request,
			"status": status, "pid": os.Getpid(), "owner": os.Getpid(), "route": route,
			"provider": "databricks", "model": "sonnet", "background": true,
			"cwd": dir, "log": filepath.Join(dir, id+".log"),
		}
		data, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			b.Fatal(err)
		}
		size := (1 + i%3) * 4096
		request["Prompt"] = brief[:size-len(data)]
		data, err = json.MarshalIndent(record, "", "  ")
		if err != nil {
			b.Fatal(err)
		}
		if len(data) != size {
			b.Fatalf("record size = %d, want %d", len(data), size)
		}
		if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0o600); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLiveRecords2000Records5Running(b *testing.B) {
	benchmarkDispatchRecords(b)
	count := 0
	visit := func(owner int, route string) {
		if owner != os.Getpid() || route != []string{"databricks:qa", "databricks:dev"}[count%2] {
			b.Fatalf("unexpected live record %d: owner=%d route=%q", count, owner, route)
		}
		count++
	}
	b.ReportAllocs()
	for b.Loop() {
		count = 0
		liveRecords(visit)
	}
	if count != 5 {
		b.Fatalf("live records = %d, want 5", count)
	}
}

func BenchmarkLiveRoutes2000Records5Running(b *testing.B) {
	benchmarkDispatchRecords(b)
	var routes map[string]int
	b.ReportAllocs()
	for b.Loop() {
		routes = LiveRoutes()
	}
	if len(routes) != 2 || routes["databricks:qa"] != 3 || routes["databricks:dev"] != 2 {
		b.Fatalf("live routes = %v, want databricks:qa=3 databricks:dev=2", routes)
	}
}
