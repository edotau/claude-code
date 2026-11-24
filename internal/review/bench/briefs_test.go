package bench

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/assets"
)

func TestBriefFixtureParity(t *testing.T) {
	fix := assets.BenchFixInput{Workdir: "/tmp/owner's lane", Package: "./internal/shell", OwnedLine: `["x"]`,
		Benchmark: "Bench", Cause: "cause", Numbers: "numbers", BeforeFile: "before", BenchCommand: "bench",
		IdentityCommand: "identity", Verify: "verify"}
	review := assets.BenchReviewInput{Workdir: fix.Workdir, Package: fix.Package, OwnedLine: fix.OwnedLine,
		Benchmark: fix.Benchmark, Numbers: fix.Numbers, BeforeFile: fix.BeforeFile, AfterFile: "after",
		IdentityCommand: fix.IdentityCommand, Verify: fix.Verify, Diff: "- old\n+ new"}
	got, err := renderFixBrief(fix)
	checkBriefHash(t, got, err, "b2f3e25aa45acad15bd8960da89d837b6abc451a34183146a9028f0a612927f0")
	got, err = renderReviewBrief(review)
	checkBriefHash(t, got, err, "2283e17c3fe8d6aa3f658f75438f4910ce7062e1a6551b2e37be02b1fbf31c48")
	review.Diff = "```diff\n\n```"
	got, err = renderReviewBrief(review)
	checkBriefHash(t, got, err, "4ff7d6f74dc7950f2b57f99b4f910ebaca2284d5a1316fc9442cf784b57cebb3")
	checkBlankFields(t, fix, review)
}

func checkBriefHash(t *testing.T, got string, err error, want string) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if hash := fmt.Sprintf("%x", sha256.Sum256([]byte(got))); hash != want {
		t.Errorf("sha256 %s, want %s", hash, want)
	}
}

func checkBlankFields(t *testing.T, fix assets.BenchFixInput, review assets.BenchReviewInput) {
	t.Helper()
	for _, item := range []any{fix, review} {
		v := reflect.ValueOf(item)
		for i := range v.NumField() {
			name := v.Type().Field(i).Name
			if name == "Diff" || v.Field(i).Kind() != reflect.String {
				continue
			}
			copy := reflect.New(v.Type()).Elem()
			copy.Set(v)
			copy.Field(i).SetString(" ")
			var err error
			switch value := copy.Interface().(type) {
			case assets.BenchFixInput:
				_, err = renderFixBrief(value)
			case assets.BenchReviewInput:
				_, err = renderReviewBrief(value)
			}
			if err == nil || !strings.Contains(err.Error(), "empty "+name) {
				t.Errorf("%T blank %s: %v", item, name, err)
			}
		}
	}
}

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
	fix := assets.BenchFixInput{Workdir: values["Workdir"], Package: values["Package"], OwnedLine: values["OwnedLine"],
		Benchmark: values["Benchmark"], Cause: values["Cause"], Numbers: values["Numbers"], BeforeFile: values["BeforeFile"],
		BenchCommand: values["BenchCommand"], IdentityCommand: values["IdentityCommand"], Verify: values["Verify"]}
	review := assets.BenchReviewInput{Workdir: values["Workdir"], Package: values["Package"], OwnedLine: values["OwnedLine"],
		Benchmark: values["Benchmark"], Numbers: values["Numbers"], BeforeFile: values["BeforeFile"],
		AfterFile: values["AfterFile"], IdentityCommand: values["IdentityCommand"], Verify: values["Verify"], Diff: values["Diff"]}
	for kind, required := range sections {
		t.Run(kind, func(t *testing.T) {
			var got string
			var err error
			if kind == "fix" {
				got, err = renderFixBrief(fix)
			} else {
				got, err = renderReviewBrief(review)
			}
			if err != nil {
				t.Fatal(err)
			}
			required = append(required, "Task:", "## Measurement evidence", "## Leaf rules", "## Reporting contract",
				values["Numbers"], values["Benchmark"], values["BeforeFile"], values["Verify"], values["IdentityCommand"],
				values["OwnedLine"], "cd '/tmp/owner'\"'\"'s lane'", "D1", "D6")
			checkBriefSections(t, got, required)
		})
	}
	checkBriefErrors(t, review)
}

func checkBriefErrors(t *testing.T, review assets.BenchReviewInput) {
	t.Helper()
	review.Diff = "```diff\n\n```"
	if got, err := renderReviewBrief(review); err != nil || !strings.Contains(got, assets.BenchNoDiff()) {
		t.Errorf("empty diff: %v, placeholder missing", err)
	}
	if got, err := renderFixBrief(assets.BenchFixInput{}); err == nil || got != "" {
		t.Errorf("fix: missing input returned %q, %v", got, err)
	}
	if got, err := renderReviewBrief(assets.BenchReviewInput{}); err == nil || got != "" {
		t.Errorf("review: missing input returned %q, %v", got, err)
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
