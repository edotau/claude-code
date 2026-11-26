package memory

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/paths"
)

// embedIndex uses the profile it is handed, not a fresh session/pin lookup.
func TestEmbedIndexUsesTheResolvedProfile(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_PROFILE", "session")
	var opened string
	prev := openEmbedClient
	t.Cleanup(func() { openEmbedClient = prev })
	openEmbedClient = func(_ context.Context, profile, _ string) (*EmbedClient, error) {
		opened = profile
		return nil, errors.New("stub")
	}
	embedIndex(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, t.TempDir(), "resolved", false)
	if opened != "resolved" {
		t.Fatalf("embedIndex opened %q, want the profile it was given", opened)
	}
}

// referenceCalls finds the named procedure readers used by memory operations.
func referenceCalls(t *testing.T, memDir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(memDir)
	if err != nil {
		t.Fatalf("read %s: %v", memDir, err)
	}
	callRe := regexp.MustCompile(`assets\.Memory([A-Za-z]+)Procedure\(\)`)
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(memDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range callRe.FindAllStringSubmatch(string(data), -1) {
			seen[m[1]] = true
		}
	}
	return seen
}

func TestProcedureReadersCoverMemoryOperations(t *testing.T) {
	seen := referenceCalls(t, ".")
	readers := map[string]func() string{"Init": assets.MemoryInitProcedure, "Update": assets.MemoryUpdateProcedure,
		"Dream": assets.MemoryDreamProcedure, "Review": assets.MemoryReviewProcedure, "Promote": assets.MemoryPromoteProcedure}
	for name, read := range readers {
		if !seen[name] || strings.TrimSpace(read()) == "" {
			t.Errorf("%s procedure lacks a real consumer or embedded text", name)
		}
	}
	if len(seen) != len(readers) {
		t.Errorf("procedure calls %v, want exactly %d named readers", seen, len(readers))
	}
}

// Given a cached notice naming a problem, When the problem is fixed and `memory index` settles, Then the cache clears.
func TestIndexRefreshesTheCachedDoctorNotice(t *testing.T) {
	tempConfig(t)
	prev := openEmbedClient
	t.Cleanup(func() { openEmbedClient = prev })
	openEmbedClient = func(context.Context, string, string) (*EmbedClient, error) { return nil, errors.New("stub") }
	decisionLog := filepath.Join(paths.MemoryStoreDir(), "decisionLog.md")
	decisionLogOver(t, decisionLog, 80)
	if err := cacheDoctorNotice(doctor(paths.MemoryStoreDir(), DefaultStaleDays, time.Now())); err != nil {
		t.Fatal(err)
	}
	write(t, decisionLog, "# D\n")
	if err := Index(&bytes.Buffer{}, &bytes.Buffer{}, IndexOptions{}); err != nil {
		t.Fatal(err)
	}
	if notice := mustRead(t, doctorNoticePath()); notice != "" {
		t.Errorf("cached notice after the fix and an index = %q, want it cleared", notice)
	}
}

// TestIndexSaysWhenThereIsNoStore: a repo without a bank dir gets a plain "no store" line, not "indexed <dir>".
func TestIndexSaysWhenThereIsNoStore(t *testing.T) {
	tempConfig(t)
	root := gitRepo(t, canonTemp(t, t.TempDir()), "bare")
	t.Chdir(root)
	var out bytes.Buffer
	if err := Index(&out, &bytes.Buffer{}, IndexOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); strings.Contains(got, "indexed ") || !strings.Contains(got, "no store") || !strings.Contains(got, "memory init") {
		t.Fatalf("index output = %q; want a no-store line naming `memory init`", got)
	}
}

func TestAppendReportLandsUnderTheBanksReports(t *testing.T) {
	prev := now
	now = func() time.Time { return time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = prev })
	runBank(t, func(t *testing.T) {
		got, err := appendReport("rev", ReportInput{Report: "# R\n", Decisions: []string{"d"}})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{paths.MemorySub("reports", "2026-11-08-rev.md"), paths.MemorySub("reports", "2026-11-08-decisions.md")}
		if !slices.Equal(got, want) {
			t.Fatalf("appendReport = %v, want %v", got, want)
		}
	})
}

func TestCheckpointCoversPendingSteward(t *testing.T) {
	tempConfig(t)
	now := time.Now()
	if err := markHandoff("session-full", now); err != nil {
		t.Fatal(err)
	}
	calls := StubSpawn(t, true)
	if !DispatchCheckpoint(UpdateRequest{SessionID: "session-full"}, true, now) || calls.Update.Load() != 0 {
		t.Fatalf("pending steward must cover checkpoint without spawning, got %d", calls.Update.Load())
	}
}

func TestStewardWaitRejectsPartialWrite(t *testing.T) {
	tempConfig(t)
	now := time.Now()
	session := "session-full"
	if err := markHandoff(session, now); err != nil {
		t.Fatal(err)
	}
	claim := takeHandoffBlock(session)
	write(t, stewardHandoffPath(session, now), "<!-- session: session-full -->\n## State\npartial")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if got := WaitStewardHandoff(ctx, session); got != "" {
		t.Fatalf("partial write released waiter: %s", got)
	}
	if err := completeHandoff(session, claim); err != nil {
		t.Fatal(err)
	}
	cleanHandoff(session, claim)
	if got := WaitStewardHandoff(context.Background(), session); got != stewardHandoffPath(session, now) {
		t.Fatalf("completed handoff not released: %s", got)
	}
}

func TestStewardRejectsCollidingSession(t *testing.T) {
	tempConfig(t)
	now := time.Now()
	write(t, handoffDoneMarker("12345678-mine"), "12345678-mine\n"+stewardHandoffPath("12345678-mine", now)+"\nrequest")
	write(t, stewardHandoffPath("12345678-mine", now), "<!-- session: 12345678-peer -->\npeer body")
	const budget = 9000
	if got := StewardHandoffBodyBudget("12345678-mine", now, budget); got != "" {
		t.Fatalf("injected colliding session: %s", got)
	}
}
