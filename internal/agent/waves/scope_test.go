package waves

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func scopeWave(agents ...string) []Spec {
	wave := make([]Spec, len(agents))
	for i, a := range agents {
		id := "1." + string(rune('1'+i))
		wave[i] = Spec{ID: id, Agent: a, Owns: []string{"/r/" + string(rune('a'+i)) + ".go"}}
	}
	return wave
}

func TestScopeFlagsDenyOtherPackagesFilesOnly(t *testing.T) {
	wave := scopeWave("opencode", "opencode")
	flags, enforced := scopeFlags(wave[0], LaunchOptions{Wave: wave})
	if !enforced {
		t.Fatal("opencode must enforce")
	}
	got := strings.Join(flags, " ")
	for _, want := range []string{"--deny Edit(/r/b.go)", "--deny Write(/r/b.go)", "--deny Edit(/r/b_test.go)", "--deny Write(/r/b.go/**)",
		"--mode auto"} {
		if !strings.Contains(got, want) {
			t.Errorf("flags %q lack %q", got, want)
		}
	}
	if strings.Contains(got, "/r/a.go") || strings.Contains(got, "/r/a_test.go") {
		t.Errorf("flags %q deny the package's own file", got)
	}
	if flags, _ = scopeFlags(wave[0], LaunchOptions{Wave: wave, Route: []string{"--mode", "plan"}}); slices.Contains(flags, "auto") {
		t.Errorf("a caller's --mode must stand: %v", flags)
	}
}

func TestScopeFlagsFallBackToDetection(t *testing.T) {
	wave := scopeWave("codex", "opencode")
	if flags, enforced := scopeFlags(wave[0], LaunchOptions{Wave: wave}); enforced || len(flags) != 0 {
		t.Errorf("codex cannot hold a path rule, got %v enforced=%v", flags, enforced)
	}
	if flags, enforced := scopeFlags(wave[0], LaunchOptions{Wave: wave[:1]}); !enforced || len(flags) != 0 {
		t.Errorf("a package alone needs no scope, got %v enforced=%v", flags, enforced)
	}
	overlap := []Spec{{ID: "1.1", Agent: "claude", Owns: []string{"/r/pkg"}}, {ID: "1.2", Agent: "claude", Owns: []string{"/r/pkg/x.go"}}}
	if flags, enforced := scopeFlags(overlap[0], LaunchOptions{Wave: overlap}); enforced || strings.Contains(strings.Join(flags, " "), "pkg/x.go") {
		t.Errorf("overlapping owns must not deny the own file and must not count as enforced: %v %v", flags, enforced)
	}
}

func TestLaunchBatchesRunUnscopedOnesAlone(t *testing.T) {
	wave := scopeWave("opencode", "codex", "claude", "codex")
	together, alone := launchBatches(wave, LaunchOptions{Wave: wave})
	if len(together) != 2 || len(alone) != 2 || alone[0].idx[0] != 1 || alone[1].idx[0] != 3 {
		t.Fatalf("together %+v alone %+v", together, alone)
	}
	args := askArgs(1, LaunchOptions{Route: []string{"--overlay", "{}"}}, together[0].flags)
	if i := slices.Index(args, "--overlay"); i < 0 || !slices.Contains(args[i:], "--deny") {
		t.Fatalf("scope flags must follow the route flags: %v", args)
	}
	if t2, a2 := launchBatches(wave[:1], LaunchOptions{Wave: wave[:1]}); len(t2) != 1 || len(a2) != 0 {
		t.Fatalf("a single package is one batch: %+v %+v", t2, a2)
	}
}

func scopeExecutable(t *testing.T) string {
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

func TestLaunchScopesEachPackageAndRunsUnscopedAlone(t *testing.T) {
	aloneGap = 0
	t.Cleanup(func() { aloneGap = checkSlack })
	exe := scopeExecutable(t)
	dir := os.Getenv("WAVES_SCOPE_HELPER")
	wave := scopeWave("opencode", "claude", "codex")
	envelopes, err := Launch(t.Context(), exe, wave, LaunchOptions{Slug: "toy", ReportsDir: t.TempDir(), Wave: wave})
	if err != nil || len(envelopes) != 3 {
		t.Fatalf("launch: %+v %v", envelopes, err)
	}
	for i, want := range []bool{true, true, false} {
		if envelopes[i].Scoped != want || envelopes[i].TaskID != "toy-"+wave[i].ID {
			t.Errorf("envelope %d: %+v, want scoped=%v", i, envelopes[i], want)
		}
	}
	for i, want := range map[string]string{"toy-1.1": "--deny\x1fEdit(/r/b.go)", "toy-1.2": "--deny\x1fWrite(/r/a.go)", "toy-1.3": "json"} {
		got, err := os.ReadFile(filepath.Join(dir, i+".args"))
		if err != nil || !strings.Contains(string(got), want) {
			t.Errorf("%s args %q (%v) lack %q", i, got, err, want)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "toy-1.3.args")); strings.Contains(string(got), "--deny") {
		t.Errorf("codex must launch with no scope flags: %q", got)
	}
}

func TestLaunchKeepsEnvelopesOfBatchesThatSucceeded(t *testing.T) {
	aloneGap = 0
	t.Cleanup(func() { aloneGap = checkSlack })
	exe := scopeExecutable(t)
	t.Setenv("WAVES_SCOPE_FAIL", "toy-1.1")
	wave := scopeWave("opencode", "opencode")
	envelopes, err := Launch(t.Context(), exe, wave, LaunchOptions{Slug: "toy", ReportsDir: t.TempDir(), Wave: wave})
	if err == nil || len(envelopes) != 1 || envelopes[0].TaskID != "toy-1.2" || !envelopes[0].Scoped {
		t.Fatalf("one batch failed whole: %+v, %v", envelopes, err)
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
