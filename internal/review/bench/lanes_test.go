package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func testLanes(t *testing.T, lanes []Lane, clean func(context.Context, string, string) (string, error)) {
	t.Helper()
	data, err := json.Marshal(lanes)
	if err != nil {
		t.Fatal(err)
	}
	oldList, oldClean := laneList, laneClean
	laneList = func(context.Context, string) (string, error) { return string(data), nil }
	laneClean = clean
	t.Cleanup(func() { laneList, laneClean = oldList, oldClean })
}

func TestAutoLanes(t *testing.T) {
	testLanes(t, []Lane{
		{Path: "/lanes/bench-providers-20261123-1", Branch: "worktree-bench-providers-20261123-1"},
		{Path: "/lanes/autofix-abcdef0", Branch: "worktree-autofix-abcdef0"},
		{Path: "/lanes/rtk-external-agents", Branch: "worktree-rtk-external-agents"},
		{Path: "/lanes/bench-providers-20261123-2", Branch: "main"},
		{Path: "/lanes/autofix-ABCDEF0", Branch: "worktree-autofix-ABCDEF0"},
	}, nil)
	got, err := AutoLanes(t.Context(), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("automatic lanes = %+v", got)
	}
	if names := []string{got[0].Name, got[1].Name}; !reflect.DeepEqual(names, []string{"bench-providers-20261123-1", "autofix-abcdef0"}) {
		t.Fatalf("automatic lanes = %+v", got)
	}
}

func TestLaneCap(t *testing.T) {
	testLanes(t, []Lane{
		{Path: "/lanes/bench-a-20261123-1", Branch: "worktree-bench-a-20261123-1"},
		{Path: "/lanes/autofix-abcdef0", Branch: "worktree-autofix-abcdef0"},
		{Path: "/lanes/bench-b-20261123-2", Branch: "worktree-bench-b-20261123-2"},
		{Path: "/lanes/gofmt-fix", Branch: "worktree-gofmt-fix"},
	}, nil)
	err := LaneCap(t.Context(), "/repo")
	if err == nil || !strings.Contains(err.Error(), "3 open") || !strings.Contains(err.Error(), "bench-b-20261123-2") {
		t.Fatalf("cap error = %v", err)
	}
}

func TestCleanLanes(t *testing.T) {
	var cleaned []string
	testLanes(t, []Lane{
		{Path: "/lanes/bench-a-20261123-1", Branch: "worktree-bench-a-20261123-1", Merged: true},
		{Path: "/lanes/autofix-abcdef0", Branch: "worktree-autofix-abcdef0", Dirty: true, Merged: true},
		{Path: "/lanes/bench-b-20261123-2", Branch: "worktree-bench-b-20261123-2", Behind: 20},
		{Path: "/lanes/bench-c-20261123-3", Branch: "worktree-bench-c-20261123-3", Behind: 21},
	}, func(_ context.Context, _, name string) (string, error) {
		cleaned = append(cleaned, name)
		return "", nil
	})
	var out bytes.Buffer
	if err := CleanLanes(t.Context(), "/repo", &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cleaned, []string{"bench-a-20261123-1"}) {
		t.Fatalf("cleaned = %v", cleaned)
	}
	if text := out.String(); !strings.Contains(text, "removed automatic lane bench-a-20261123-1") ||
		!strings.Contains(text, "kept dirty automatic lane autofix-abcdef0") ||
		!strings.Contains(text, "claude-code worktree merge bench-c-20261123-3") ||
		strings.Contains(text, "bench-b-20261123-2") {
		t.Fatalf("notices = %q", text)
	}
}

func TestCleanLanesFailures(t *testing.T) {
	want := errors.New("refused")
	testLanes(t, []Lane{{Path: "/lanes/autofix-abcdef0", Branch: "worktree-autofix-abcdef0", Merged: true}},
		func(context.Context, string, string) (string, error) { return "refused: still in use\n", want })
	var out bytes.Buffer
	if err := CleanLanes(t.Context(), "/repo", &out); !errors.Is(err, want) ||
		out.String() != "refused automatic lane autofix-abcdef0: refused: still in use\n" {
		t.Fatalf("error = %v, notice = %q", err, out.String())
	}
	laneList = func(context.Context, string) (string, error) { return "", want }
	out.Reset()
	if err := CleanLanes(t.Context(), "/repo", &out); !errors.Is(err, want) || out.Len() != 0 {
		t.Fatalf("list error = %v, notices = %q", err, out.String())
	}
}
