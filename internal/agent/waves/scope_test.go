package waves

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/parallel"
)

func scopeWave(agents ...string) []Spec {
	wave := make([]Spec, len(agents))
	for i, a := range agents {
		id := "1." + string(rune('1'+i))
		wave[i] = Spec{ID: id, Agent: a, Owns: []string{"/r/" + string(rune('a'+i)) + ".go"}}
	}
	return wave
}

func TestLaunchBatchesRunSiblingsAlone(t *testing.T) {
	wave := scopeWave("opencode", "codex", "claude")
	together, alone := launchBatches(wave, LaunchOptions{Wave: wave})
	if len(together) != 0 || len(alone) != 3 || alone[1].idx[0] != 1 {
		t.Fatalf("together %+v alone %+v", together, alone)
	}
	if t2, a2 := launchBatches(wave[:1], LaunchOptions{Wave: wave[:1]}); len(t2) != 1 || len(a2) != 0 || !t2[0].enforced {
		t.Fatalf("a single package is one scoped batch: %+v %+v", t2, a2)
	}
}

func scopeExecutable(t *testing.T) string {
	hosttest.SkipOnWindows(t, "sh script")
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ask")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(self, "'", "'\\''") + "' -test.run=^TestScopeHelper$ -- \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAVES_SCOPE_HELPER", t.TempDir())
	return path
}

// TestScopeHelper stands in for `claude-code`: `agents ask` records its args and echoes one ok envelope per line.
func TestScopeHelper(t *testing.T) {
	dir := os.Getenv("WAVES_SCOPE_HELPER")
	if dir == "" {
		return
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	if args[1] == "result" {
		_ = os.WriteFile(args[4], nil, 0o600)
		fmt.Printf("{\"path\":%q}\n", args[4])
		os.Exit(0)
	}
	var id string
	var line launchLine
	if json.NewDecoder(os.Stdin).Decode(&line) != nil {
		os.Exit(2)
	}
	id = line.ID
	if id == os.Getenv("WAVES_SCOPE_FAIL") {
		os.Exit(2)
	}
	if path := os.Getenv("WAVES_SCOPE_WRITE"); path != "" && id == "toy-1.1" {
		_ = os.WriteFile(path, []byte(time.Now().String()), 0o600) // the leaf touches another package's file
	}
	_ = os.WriteFile(filepath.Join(dir, id+".args"), []byte(strings.Join(args, "\x1f")), 0o600)
	fmt.Printf("{\"task_id\":%q,\"dispatch_id\":\"job-%s\",\"status\":\"ok\"}\n", id, id)
	os.Exit(0)
}

func TestLaunchRunsSiblingsAloneUnscoped(t *testing.T) {
	aloneGap = 0
	t.Cleanup(func() { aloneGap = checkSlack })
	exe := scopeExecutable(t)
	dir := os.Getenv("WAVES_SCOPE_HELPER")
	wave := scopeWave("opencode", "claude", "codex")
	envelopes, err := Launch(t.Context(), exe, wave, LaunchOptions{Slug: "toy", ReportsDir: t.TempDir(), Wave: wave})
	if err != nil || len(envelopes) != 3 {
		t.Fatalf("launch: %+v %v", envelopes, err)
	}
	for i := range wave {
		if envelopes[i].Scoped || envelopes[i].TaskID != "toy-"+wave[i].ID {
			t.Errorf("envelope %d: %+v, want unscoped", i, envelopes[i])
		}
		if got, _ := os.ReadFile(filepath.Join(dir, "toy-"+wave[i].ID+".args")); strings.Contains(string(got), "--permissions") {
			t.Errorf("%s launched with a permission flag: %q", wave[i].ID, got)
		}
	}
}

func TestLaunchKeepsEnvelopesOfBatchesThatSucceeded(t *testing.T) {
	aloneGap = 0
	t.Cleanup(func() { aloneGap = checkSlack })
	exe := scopeExecutable(t)
	t.Setenv("WAVES_SCOPE_FAIL", "toy-1.1")
	wave := scopeWave("opencode", "opencode")
	envelopes, err := Launch(t.Context(), exe, wave, LaunchOptions{Slug: "toy", ReportsDir: t.TempDir(), Wave: wave})
	if err == nil || len(envelopes) != 2 {
		t.Fatalf("one batch failed whole: %+v, %v", envelopes, err)
	}
	if got := [3]string{envelopes[0].TaskID, envelopes[0].Status, envelopes[1].TaskID}; got != [3]string{"toy-1.1", "error", "toy-1.2"} {
		t.Fatalf("batch failure lost a sibling: %+v", envelopes)
	}
}

func TestLaunchNamesAnUnscopedLeafThatWroteAnotherPackagesFile(t *testing.T) {
	aloneGap = 0
	t.Cleanup(func() { aloneGap = checkSlack })
	exe := scopeExecutable(t)
	dir := t.TempDir()
	foreign := filepath.Join(dir, "b.go")
	t.Setenv("WAVES_SCOPE_WRITE", foreign)
	wave := []Spec{{ID: "1.1", Agent: "codex", Owns: []string{filepath.Join(dir, "a.go")}},
		{ID: "1.2", Agent: "codex", Owns: []string{foreign}}}
	fp := func(p string) (string, error) { b, _ := os.ReadFile(p); return string(b), nil }
	envelopes, err := Launch(t.Context(), exe, wave, LaunchOptions{Slug: "toy", ReportsDir: t.TempDir(), Wave: wave, Fingerprint: fp})
	if err != nil || len(envelopes) != 2 {
		t.Fatalf("launch: %+v %v", envelopes, err)
	}
	want := CheckViolation{Package: "1.1", File: foreign, Owners: []string{"1.2"}}
	if len(envelopes[0].Foreign) != 1 || !reflect.DeepEqual(envelopes[0].Foreign[0], want) || len(envelopes[1].Foreign) != 0 {
		t.Fatalf("foreign writes: %+v / %+v, want %+v from 1.1 only", envelopes[0].Foreign, envelopes[1].Foreign, want)
	}
}

func TestLaunchAllContainsPanic(t *testing.T) {
	original, gap := scopeBatchRun, aloneGap
	t.Cleanup(func() { scopeBatchRun, aloneGap = original, gap })
	aloneGap = 0
	for _, agents := range [][]string{{"opencode", "claude", "opencode"}, {"codex", "codex", "codex"}} {
		wave := scopeWave(agents...)
		scopeBatchRun = func(_ context.Context, _ string, part []Spec, _ LaunchOptions) ([]Envelope, error) {
			if part[0].ID == wave[1].ID {
				panic("batch exploded")
			}
			return []Envelope{{TaskID: part[0].ID, Status: "ok", DispatchID: "job"}}, nil
		}
		results := launchAll(t.Context(), "must-not-run", wave, LaunchOptions{Wave: wave})
		if len(results) != len(wave) {
			t.Fatalf("results: %+v", results)
		}
		var pe *parallel.PanicError
		if !errors.As(results[1].err, &pe) || pe.Value != "batch exploded" || len(pe.Stack) == 0 {
			t.Fatalf("panic slot: %+v", results[1])
		}
		results[1].err = nil
		for i, res := range results {
			var want []Envelope
			if i != 1 {
				want = []Envelope{{TaskID: wave[i].ID, Status: "ok", DispatchID: "job"}}
			}
			if !slotIs(res, i, want) {
				t.Fatalf("slot %d: %+v, want envelopes %+v", i, res, want)
			}
		}
	}
}

// slotIs is whether res is batch i's clean result carrying want.
func slotIs(res batchResult, i int, want []Envelope) bool {
	return res.err == nil && res.snapErr == nil && len(res.foreign) == 0 && slices.Equal(res.batch.idx, []int{i}) &&
		reflect.DeepEqual(res.envelopes, want)
}
