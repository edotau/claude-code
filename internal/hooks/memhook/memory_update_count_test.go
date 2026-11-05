package memory

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory/bank"
	"github.com/edotau/claude-code/internal/paths"
)

// statSize returns path's current size, failing the test when it cannot be stat'ed.
func statSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// TestCountEditsIncludesSubagentEdits: a dispatched subagent's own agent-*.jsonl transcript counts toward
// the parent session's edits, and that combined total is what arms the Stop update gate.
func TestCountEditsIncludesSubagentEdits(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(bank.UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeEdits(t, tr, 1)
	subDir := filepath.Join(strings.TrimSuffix(tr, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEdits(t, filepath.Join(subDir, "agent-a.jsonl"), 2)

	if got := countEdits(tr, countPath("sub-session")); got != 3 {
		t.Fatalf("countEdits = %d, want 3 (1 main edit + 2 subagent edits)", got)
	}

	cwd := t.TempDir()
	calls := StubSpawnForTest(t, true)
	payload := fmt.Sprintf(`{"session_id":"sub-session","cwd":%q,"transcript_path":%q}`, cwd, tr)
	if code := sessionUpdateCode(core.ParseInput(strings.NewReader(payload)), &bytes.Buffer{}); code != core.ExitProceed || calls.Load() != 1 {
		t.Fatalf("subagent edits must arm the update gate: code=%d calls=%d", code, calls.Load())
	}
}

// TestCountEditsGitCommitWeightArmsGate: a single `git commit` Bash call, with zero Edit/Write calls,
// counts for updateMinEdits on its own and arms the Stop gate.
func TestCountEditsGitCommitWeightArmsGate(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(bank.UpdateChildEnv, "")
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	commit := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git commit -m x"}}]}}` + "\n"
	if err := os.WriteFile(tr, []byte(commit), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := countEdits(tr, countPath("commit-session")); got != updateMinEdits {
		t.Fatalf("countEdits = %d, want %d (one git commit)", got, updateMinEdits)
	}

	cwd := t.TempDir()
	calls := StubSpawnForTest(t, true)
	payload := fmt.Sprintf(`{"session_id":"commit-session","cwd":%q,"transcript_path":%q}`, cwd, tr)
	if code := sessionUpdateCode(core.ParseInput(strings.NewReader(payload)), &bytes.Buffer{}); code != core.ExitProceed || calls.Load() != 1 {
		t.Fatalf("a bare git commit must arm the update gate: code=%d calls=%d", code, calls.Load())
	}
}

// TestCountStateOldFormatReadsAndNewFieldsRoundTrip: a pre-upgrade "edits|offset" count file forces a
// full rescan from 0 (its offset sits past commit lines the old binary never weighted), and
// writeCountState's 3-field format round-trips exactly.
func TestCountStateOldFormatReadsAndNewFieldsRoundTrip(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "count-x")
	if err := os.WriteFile(path, []byte("5|120"), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, ok := readCountState(path)
	if !ok || cs != (countState{}) {
		t.Fatalf("old-format read = %+v ok=%v, want zero state forcing a full rescan", cs, ok)
	}

	want := countState{edits: 7, offset: 240, subEdits: 4, subBytes: 999}
	writeCountState(path, want)
	got, ok := readCountState(path)
	if !ok || got != want {
		t.Fatalf("round trip = %+v ok=%v, want %+v", got, ok, want)
	}
}

// TestCountEditsOldFormatCountFileRescansUnweightedCommit: an old 2-field count file's offset already sits
// past a `git commit` the old binary never weighted. Upgrading must force a full rescan from 0 so that
// commit is counted at CommitEditWeight, and the count file is rewritten in the new 4-field format.
func TestCountEditsOldFormatCountFileRescansUnweightedCommit(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	commit := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git commit -m x"}}]}}` + "\n"
	if err := os.WriteFile(tr, []byte(commit), 0o600); err != nil {
		t.Fatal(err)
	}
	countFile := filepath.Join(t.TempDir(), "count-old")
	if err := os.WriteFile(countFile, fmt.Appendf(nil, "0|%d", len(commit)), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := countEdits(tr, countFile); got != updateMinEdits {
		t.Fatalf("countEdits after old-format upgrade = %d, want %d (the commit, now weighted)", got, updateMinEdits)
	}
	raw, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	if parts := strings.Split(strings.TrimSpace(string(raw)), "|"); len(parts) != 4 {
		t.Fatalf("count file after upgrade = %q, want the 4-field format", raw)
	}
}

// TestCountEditsThreeFieldCountFileRescansTurns: a 3-field pre-upgrade count file's offset sits past
// user turns that format never counted, so upgrading must force a full rescan from 0 — the turns are
// re-weighted — and the count file is rewritten in the 4-field format.
func TestCountEditsThreeFieldCountFileRescansTurns(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tr := filepath.Join(t.TempDir(), "t.jsonl")
	writeUserTurns(t, tr, 4)
	fi, err := os.Stat(tr)
	if err != nil {
		t.Fatal(err)
	}
	countFile := filepath.Join(t.TempDir(), "count-3field")
	// Pre-upgrade "edits|offset|subEdits:subBytes": offset past every (uncounted) user turn.
	if err := os.WriteFile(countFile, fmt.Appendf(nil, "0|%d|0:0", fi.Size()), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := countEdits(tr, countFile); got != 2 { // 4 turns / 2 per work unit
		t.Fatalf("countEdits after 3-field upgrade = %d, want 2 (the 4 turns, now weighted)", got)
	}
	raw, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	if parts := strings.Split(strings.TrimSpace(string(raw)), "|"); len(parts) != 4 {
		t.Fatalf("count file after upgrade = %q, want the 4-field format", raw)
	}
}

// TestCountSubagentEditsCachesUnchangedSizeRecountsGrowth: subagentEdits trusts a cached edit count
// outright when the summed subagent file size hasn't moved, and only rescans once it grows.
func TestCountSubagentEditsCachesUnchangedSizeRecountsGrowth(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "t.jsonl")
	subDir := filepath.Join(strings.TrimSuffix(tr, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(subDir, "agent-a.jsonl")
	writeEdits(t, agent, 2)
	size := statSize(t, agent)

	edits, bytes := subagentEdits(tr, 0, 0, true)
	if edits != 2 || bytes != size {
		t.Fatalf("initial scan = (%d,%d), want (2,%d)", edits, bytes, size)
	}

	// A deliberately wrong cached edit count, paired with the SAME cached size, must be trusted as-is:
	// this is the only way to prove the unchanged-size path skips the rescan rather than coincidentally
	// recomputing the same answer.
	edits2, bytes2 := subagentEdits(tr, size, 999, true)
	if edits2 != 999 || bytes2 != size {
		t.Fatalf("unchanged size must skip the rescan: got (%d,%d), want (999,%d)", edits2, bytes2, size)
	}

	writeEdits(t, agent, 5) // grows the file: the cached size no longer matches
	size2 := statSize(t, agent)
	if size2 == size {
		t.Fatal("test setup: file did not grow")
	}
	edits3, bytes3 := subagentEdits(tr, size, 999, true)
	if edits3 != 5 || bytes3 != size2 {
		t.Fatalf("grown file must be rescanned: got (%d,%d), want (5,%d)", edits3, bytes3, size2)
	}
}

// wantSidecarEdits fails unless every listed file's sidecar entry records exactly the wanted edit count.
func wantSidecarEdits(t *testing.T, sc map[string]subFileState, want map[string]int) {
	t.Helper()
	for path, edits := range want {
		if sc[path].Edits != edits {
			t.Fatalf("sidecar entry for %s = %+v, want %d edits (sidecar %+v)", filepath.Base(path), sc[path], edits, sc)
		}
	}
}

// subagentFixture returns a fresh session transcript path + its subagents dir, created on disk.
func subagentFixture(t *testing.T) (tr, subDir string) {
	t.Helper()
	dir := t.TempDir()
	tr = filepath.Join(dir, "t.jsonl")
	subDir = filepath.Join(strings.TrimSuffix(tr, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return tr, subDir
}

// TestCountSubagentEditsGrowOneFileOnlyAdvancesItsSidecarEntry: with 3 sibling files, growing one must
// only rescan (and advance the sidecar offset of) that file — the other two entries come back unchanged.
func TestCountSubagentEditsGrowOneFileOnlyAdvancesItsSidecarEntry(t *testing.T) {
	tr, subDir := subagentFixture(t)
	a := filepath.Join(subDir, "agent-a.jsonl")
	b := filepath.Join(subDir, "agent-b.jsonl")
	c := filepath.Join(subDir, "agent-c.jsonl")
	writeEdits(t, a, 1)
	writeEdits(t, b, 2)
	writeEdits(t, c, 3)

	edits, totalBytes := subagentEdits(tr, 0, 0, true)
	if want := 1 + 2 + 3; edits != want {
		t.Fatalf("initial scan edits = %d, want %d", edits, want)
	}
	sidecar := sidecarPath(subDir)
	before := readSidecar(sidecar)
	wantSidecarEdits(t, before, map[string]int{a: 1, b: 2, c: 3})

	writeEdits(t, a, 4) // only a grows (overwritten with more lines, same identical-line trick as above)
	size := statSize(t, a)

	edits2, totalBytes2 := subagentEdits(tr, totalBytes, edits, true)
	if totalBytes2 == totalBytes {
		t.Fatal("test setup: total size did not move")
	}
	if want := 4 + 2 + 3; edits2 != want {
		t.Fatalf("after growing a: edits = %d, want %d", edits2, want)
	}
	after := readSidecar(sidecar)
	wantSidecarEdits(t, after, map[string]int{a: 4})
	if after[a].Offset != size {
		t.Fatalf("grown file a's sidecar entry = %+v, want offset %d", after[a], size)
	}
	if after[b] != before[b] || after[c] != before[c] {
		t.Fatalf("sibling sidecar entries moved: b=%+v (was %+v), c=%+v (was %+v)", after[b], before[b], after[c], before[c])
	}
}

// TestCountSubagentEditsOffsetPastFileSizeRescansFromZero: a sidecar offset larger than the file's
// current size (truncation/rotation) must rescan that file from 0, not skip it or go negative.
func TestCountSubagentEditsOffsetPastFileSizeRescansFromZero(t *testing.T) {
	tr, subDir := subagentFixture(t)
	a := filepath.Join(subDir, "agent-a.jsonl")
	writeEdits(t, a, 3)
	fi, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	writeSidecar(sidecarPath(subDir), map[string]subFileState{a: {Offset: fi.Size() + 1_000, Edits: 999}})

	edits, totalBytes := subagentEdits(tr, 0, 0, true) // cachedBytes=0 forces the rescan path
	if edits != 3 || totalBytes != fi.Size() {
		t.Fatalf("stale-offset rescan = (%d,%d), want (3,%d)", edits, totalBytes, fi.Size())
	}
	got := readSidecar(sidecarPath(subDir))
	if got[a].Edits != 3 || got[a].Offset != fi.Size() {
		t.Fatalf("sidecar after stale-offset rescan = %+v, want 3 edits at offset %d", got[a], fi.Size())
	}
}

// TestCountSubagentEditsGarbageSidecarTreatedAsEmpty: unparsable sidecar JSON must not panic and must
// behave as if no cache existed — every file rescanned from 0.
func TestCountSubagentEditsGarbageSidecarTreatedAsEmpty(t *testing.T) {
	tr, subDir := subagentFixture(t)
	a := filepath.Join(subDir, "agent-a.jsonl")
	writeEdits(t, a, 2)
	if err := os.WriteFile(sidecarPath(subDir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	edits, totalBytes := subagentEdits(tr, 0, 0, true)
	fi, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	if edits != 2 || totalBytes != fi.Size() {
		t.Fatalf("garbage-sidecar scan = (%d,%d), want (2,%d)", edits, totalBytes, fi.Size())
	}
	got := readSidecar(sidecarPath(subDir))
	if got[a].Edits != 2 {
		t.Fatalf("sidecar rewritten after garbage input = %+v, want 2 edits for a", got[a])
	}
}

// TestCountSubagentEditsDeletedFileDropsFromSidecar: a subagent file removed between Stops must not
// survive in the rewritten sidecar (it would otherwise leak a stale entry forever).
func TestCountSubagentEditsDeletedFileDropsFromSidecar(t *testing.T) {
	tr, subDir := subagentFixture(t)
	a := filepath.Join(subDir, "agent-a.jsonl")
	b := filepath.Join(subDir, "agent-b.jsonl")
	writeEdits(t, a, 1)
	writeEdits(t, b, 2)

	edits, totalBytes := subagentEdits(tr, 0, 0, true)
	if edits != 3 {
		t.Fatalf("initial scan edits = %d, want 3", edits)
	}
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	writeEdits(t, b, 5) // move the total size so the cache is invalidated and a rescan actually runs

	edits2, totalBytes2 := subagentEdits(tr, totalBytes, edits, true)
	if totalBytes2 == totalBytes {
		t.Fatal("test setup: total size did not move")
	}
	if edits2 != 5 {
		t.Fatalf("after deleting a: edits = %d, want 5 (b only)", edits2)
	}
	got := readSidecar(sidecarPath(subDir))
	if _, present := got[a]; present {
		t.Fatalf("deleted file a still present in rewritten sidecar: %+v", got)
	}
	if _, present := got[b]; !present {
		t.Fatalf("surviving file b missing from rewritten sidecar: %+v", got)
	}
}

// TestCountSubagentEditsConcurrentCallsCountOnce: Stop and SessionEnd can call in for the same session at
// once; two concurrent calls racing the same growth must serialize through the sidecar lock so the total
// is counted once each, not doubled, and both land on the same final offset.
func TestCountSubagentEditsConcurrentCallsCountOnce(t *testing.T) {
	tr, subDir := subagentFixture(t)
	a := filepath.Join(subDir, "agent-a.jsonl")
	writeEdits(t, a, 2)

	edits0, bytes0 := subagentEdits(tr, 0, 0, true) // seed the sidecar
	if edits0 != 2 {
		t.Fatalf("seed scan edits = %d, want 2", edits0)
	}

	writeEdits(t, a, 7) // grows past the cached size: both goroutines will try the lock
	fi, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make([]struct {
		edits int
		bytes int64
	}, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e, b := subagentEdits(tr, bytes0, edits0, true)
			results[i] = struct {
				edits int
				bytes int64
			}{e, b}
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r.edits != 7 || r.bytes != fi.Size() {
			t.Errorf("goroutine %d = (%d,%d), want (7,%d) — the race must not double- or under-count", i, r.edits, r.bytes, fi.Size())
		}
	}
	got := readSidecar(sidecarPath(subDir))
	if got[a].Edits != 7 || got[a].Offset != fi.Size() {
		t.Fatalf("sidecar after concurrent calls = %+v, want 7 edits at offset %d (one writer, consistent offset)", got[a], fi.Size())
	}
}

// TestCountSubagentEditsLockHeldElsewhereReturnsCachedWithoutWriting: when the sidecar lock is held by
// another writer, subagentEdits must fall back to the caller's cached totals, write nothing, and
// return within its (test-shortened) lock wait rather than hang.
func TestCountSubagentEditsLockHeldElsewhereReturnsCachedWithoutWriting(t *testing.T) {
	oldWait := sidecarLockWait
	sidecarLockWait = 100 * time.Millisecond
	t.Cleanup(func() { sidecarLockWait = oldWait })
	tr, subDir := subagentFixture(t)
	a := filepath.Join(subDir, "agent-a.jsonl")
	writeEdits(t, a, 2)
	edits0, bytes0 := subagentEdits(tr, 0, 0, true) // seed the sidecar
	before := readSidecar(sidecarPath(subDir))

	writeEdits(t, a, 9) // grows past the cached size: subagentEdits must attempt the lock

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		paths.WithFileLock(sidecarPath(subDir), func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer close(release)

	start := time.Now()
	edits, bytes := subagentEdits(tr, bytes0, edits0, true)
	elapsed := time.Since(start)

	// The lock-busy path keeps the caller's cached totals entirely (edits AND bytes): caching the new
	// totalBytes against stale edits would make the next call's fast-path skip the recount permanently.
	if edits != edits0 || bytes != bytes0 {
		t.Fatalf("lock held elsewhere: subagentEdits = (%d,%d), want (%d,%d)", edits, bytes, edits0, bytes0)
	}
	if elapsed > time.Second {
		t.Fatalf("subagentEdits took %s waiting on a held lock, want it to give up near its %s wait", elapsed, sidecarLockWait)
	}
	after := readSidecar(sidecarPath(subDir))
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("sidecar written while the lock was held elsewhere: before=%+v after=%+v", before, after)
	}
}

// TestReadOnlyEditsMatchesCountEditsWithoutWriting: the dry-run count includes subagent edits and persists nothing.
func TestReadOnlyEditsMatchesCountEditsWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	tr := filepath.Join(dir, "t.jsonl")
	writeEdits(t, tr, 1)
	subDir := filepath.Join(strings.TrimSuffix(tr, ".jsonl"), "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEdits(t, filepath.Join(subDir, "agent-a.jsonl"), 3)
	countFile := filepath.Join(dir, "count")

	dry := readOnlyEdits(tr, countFile)
	if entries, _ := os.ReadDir(subDir); len(entries) != 1 {
		t.Errorf("dry count wrote into the subagents dir: %v", entries)
	}
	if _, err := os.Stat(countFile); err == nil {
		t.Error("dry count wrote the count file")
	}
	if real := countEdits(tr, countFile); dry != 4 || real != dry {
		t.Errorf("dry = %d, real = %d, want both 4", dry, real)
	}
}
