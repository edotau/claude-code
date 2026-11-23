package autofix

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func setupLeaf(t *testing.T, launch func(context.Context, string, string, []string) (string, error)) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "attempts.jsonl")
	previousFile, previousLaunch := attemptFile, launchLeaf
	attemptFile = func() string { return file }
	launchLeaf = launch
	t.Cleanup(func() { attemptFile, launchLeaf = previousFile, previousLaunch })
	return file
}

func leafRequest() LeafRequest {
	return LeafRequest{Sha: "1234567", Lane: "/tmp/autofix-1234567", Live: "/repo/live",
		Gate: "rtk go test ./internal/review/autofix", GateOutput: "FAIL complexity: too many branches",
		Findings: []Finding{
			{Class: ClassComplexity, Path: "internal/a.go", Line: 12, Rule: "cyclop", Message: "too complex"},
			{Class: ClassIneffassign, Path: "internal/b.go", Line: 7, Rule: "ineffassign", Message: "unused assignment"},
			{Class: ClassGofmt, Path: "internal/c.go"},
		}}
}

func assertBriefContains(t *testing.T, brief string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(brief, want) {
			t.Errorf("brief missing %q: %s", want, brief)
		}
	}
}

func readAttempt(t *testing.T, file string) Attempt {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var row Attempt
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestFixLeafBriefAndRecord(t *testing.T) {
	file := setupLeaf(t, func(_ context.Context, dir, brief string, args []string) (string, error) {
		if dir != "/tmp/autofix-1234567" || !slices.Equal(args, LeafArgs(leafRequest())) {
			t.Errorf("launch dir or args: %q %v", dir, args)
		}
		assertBriefContains(t, brief, `Owned: ["internal/a.go","internal/b.go"]`,
			"Verify: rtk go test ./internal/review/autofix", "internal/a.go:12 complexity",
			"internal/b.go:7 ineffassign", "FAIL complexity: too many branches")
		if strings.Contains(brief, "internal/c.go") {
			t.Error("deterministic finding leaked into brief")
		}
		return "job-123", nil
	})
	row, err := FixLeaf(t.Context(), leafRequest(), func() (bool, string) { return true, "" })
	if err != nil || row.Verdict != "dispatched" || row.DispatchID != "job-123" {
		t.Fatalf("FixLeaf = %+v, %v", row, err)
	}
	saved := readAttempt(t, file)
	if saved.Key != row.Key || !slices.Equal(saved.Paths, []string{"internal/a.go", "internal/b.go"}) {
		t.Fatalf("attempt row = %+v", saved)
	}
}

func TestFixLeafAttempted(t *testing.T) {
	calls := 0
	setupLeaf(t, func(context.Context, string, string, []string) (string, error) {
		calls++
		return "job-123", nil
	})
	req := leafRequest()
	allow := func() (bool, string) { return true, "" }
	if _, err := FixLeaf(t.Context(), req, allow); err != nil {
		t.Fatal(err)
	}
	req.Findings = slices.Clone(req.Findings)
	slices.Reverse(req.Findings)
	row, err := FixLeaf(t.Context(), req, allow)
	if !errors.Is(err, ErrAttempted) || row.DispatchID != "job-123" || calls != 1 {
		t.Fatalf("second attempt = %+v, %v; launches %d", row, err, calls)
	}
}

func TestFixLeafBudget(t *testing.T) {
	file := setupLeaf(t, func(context.Context, string, string, []string) (string, error) {
		t.Fatal("launched despite budget refusal")
		return "", nil
	})
	_, err := FixLeaf(t.Context(), leafRequest(), func() (bool, string) { return false, "15M tokens used today" })
	if !errors.Is(err, ErrBudget) || !strings.Contains(err.Error(), "15M tokens used today") {
		t.Fatalf("budget error = %v", err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("attempt record after refusal: %v", err)
	}
}
