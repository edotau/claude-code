package bench

import (
	"maps"
	"strings"
	"testing"
)

func TestRenderBrief(t *testing.T) {
	values := map[string]string{
		"Workdir": "/tmp/owner's lane", "Package": "./internal/shell",
		"OwnedLine": `["/tmp/owner's lane/internal/shell/shell.go"]`,
		"Benchmark": "BenchmarkCommentsEachHash", "Cause": "repeated buffer allocation",
		"Numbers":    "before: 120 ns/op, 64 B/op; after: 90 ns/op, 32 B/op",
		"BeforeFile": "/tmp/before.txt", "AfterFile": "/tmp/after.txt",
		"BenchCommand":    "rtk go test -run '^$' -bench '^BenchmarkCommentsEachHash$' -benchmem ./internal/shell",
		"IdentityCommand": "rtk go test -run '^TestCommentsEachHash$' ./internal/shell",
		"Verify":          "rtk go test ./internal/shell", "Diff": "- old allocation\n+ reused buffer",
	}
	sections := map[string][]string{
		"fix": {"Mode: implementation", "## Preserve behaviour", "never skip or drop a validation",
			"*_test.go unchanged", "hand back uncommitted", values["Cause"], values["BenchCommand"]},
		"review": {"Owned: []", "Mode: read-only", "Opus or higher", "## Fix diff", "## Behaviour review",
			"KEEP or REVERT", "OWNER REVIEW REQUIRED", "a stale cache", "a race", values["Diff"], values["AfterFile"]},
	}
	for kind, required := range sections {
		t.Run(kind, func(t *testing.T) {
			got, err := RenderBrief(kind, values)
			if err != nil {
				t.Fatal(err)
			}
			required = append(required, "Task:", "## Measurement evidence", "## Leaf rules", "## Reporting contract",
				values["Numbers"], values["Benchmark"], values["BeforeFile"], values["Verify"], values["IdentityCommand"],
				values["OwnedLine"], "cd '/tmp/owner'\"'\"'s lane'", "D1", "D6")
			checkBriefSections(t, got, required)
		})
	}
	empty := maps.Clone(values)
	empty["Diff"] = "```diff\n\n```"
	if got, err := RenderBrief("review", empty); err != nil || !strings.Contains(got, noDiff) {
		t.Errorf("empty diff: %v, placeholder missing", err)
	}
	for _, kind := range []string{"fix", "review", "unknown"} {
		if got, err := RenderBrief(kind, nil); err == nil || got != "" {
			t.Errorf("%s: missing input returned %q, %v", kind, got, err)
		}
	}
}

func checkBriefSections(t *testing.T, got string, required []string) {
	t.Helper()
	for _, want := range required {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(got, "{{") || strings.Contains(got, "<no value>") {
		t.Error("unrendered placeholder")
	}
}
