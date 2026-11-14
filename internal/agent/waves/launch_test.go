package waves

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func launchExecutable(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ask")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestLaunchHelper$ -- \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAVES_LAUNCH_HELPER", "1")
	return path
}

func TestLaunchHelper(t *testing.T) {
	if os.Getenv("WAVES_LAUNCH_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	args = args[1:]
	if len(args) < 3 || args[0] != "agents" {
		os.Exit(2)
	}
	if args[1] == "result" {
		launchHelperSave(args)
	}

	want := []string{"agents", "ask", "--batch", "-", "--jobs", os.Getenv("WAVES_LAUNCH_JOBS"), "--format", "json"}
	if route := os.Getenv("WAVES_LAUNCH_ROUTE"); route != "" {
		want = append(want, strings.Split(route, "\x1f")...)
	}
	if !reflect.DeepEqual(args, want) {
		os.Exit(5)
	}
	lines := launchHelperLines()
	for i, line := range slices.Backward(lines) {
		fmt.Printf("{\"task_id\":%q,\"dispatch_id\":%q,\"status\":%q}\n", line.ID, "job-"+line.ID, launchHelperStatus(i))
	}
	if len(lines) > 1 {
		os.Exit(1)
	}
	os.Exit(0)
}

func launchHelperStatus(i int) string {
	if i == 1 {
		return "error"
	}
	return "ok"
}

func launchHelperLines() []launchLine {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(6)
	}
	if err := os.WriteFile(os.Getenv("WAVES_LAUNCH_STDIN"), data, 0600); err != nil {
		os.Exit(7)
	}
	var lines []launchLine
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		var line launchLine
		if json.Unmarshal(scanner.Bytes(), &line) != nil {
			os.Exit(8)
		}
		lines = append(lines, line)
	}
	if scanner.Err() != nil {
		os.Exit(9)
	}
	return lines
}

func launchHelperSave(args []string) {
	if len(args) != 7 || args[3] != "--save" {
		os.Exit(3)
	}
	if err := os.WriteFile(args[4], []byte(args[2]), 0600); err != nil {
		os.Exit(4)
	}
	fmt.Printf("{\"path\":%q}\n", args[4])
	if strings.HasSuffix(args[2], "1.2") {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestLaunchLive(t *testing.T) {
	executable := launchExecutable(t)
	dir := t.TempDir()
	capture := filepath.Join(dir, "stdin.jsonl")
	t.Setenv("WAVES_LAUNCH_STDIN", capture)
	t.Setenv("WAVES_LAUNCH_JOBS", "3")
	tools := true
	options := LaunchOptions{Slug: "toy", ReportsDir: dir, Profiles: []string{"dev", "qa"}, Tools: &tools, Timeout: time.Minute}
	specs := []Spec{{ID: "1.1", Agent: "codex", Task: "first", Owns: []string{"/tmp/first.go"}, Workdir: dir, Verify: "go vet"},
		{ID: "1.2", Agent: "codex", Task: "second"}, {ID: "1.3", Agent: "codex", Task: "third"}}
	envelopes, err := Launch(t.Context(), executable, specs, options)
	if err == nil {
		t.Fatal("failed batch exit was lost")
	}
	if len(envelopes) != len(specs) {
		t.Fatalf("envelopes: %+v, %v", envelopes, err)
	}
	launchCheckEnvelopes(t, envelopes, specs)
	launchCheckLines(t, launchReadCapture(t, capture), specs, options)
}

func launchCheckEnvelopes(t *testing.T, envelopes []Envelope, specs []Spec) {
	t.Helper()
	for i, envelope := range envelopes {
		if envelope.TaskID != "toy-"+specs[i].ID || envelope.DispatchID != "job-"+envelope.TaskID {
			t.Fatalf("mapping: %+v", envelope)
		}
		if envelope.Status != launchHelperStatus(i) {
			t.Fatalf("status: %+v", envelope)
		}
		data, err := os.ReadFile(envelope.Result)
		if err != nil || string(data) != envelope.DispatchID {
			t.Fatalf("save: %q, %v", data, err)
		}
	}
}

func launchReadCapture(t *testing.T, path string) []launchLine {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []launchLine
	for raw := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var line launchLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	return lines
}

func launchCheckLines(t *testing.T, lines []launchLine, specs []Spec, options LaunchOptions) {
	t.Helper()
	for i, line := range lines {
		launchCheckLine(t, line, specs[i], options.Profiles[i%2], options.ReportsDir)
	}
	if !strings.Contains(lines[0].Prompt, `Owned: ["/tmp/first.go"]`) || !strings.Contains(lines[0].Prompt, "Verify: go vet") || lines[0].Workdir != specs[0].Workdir {
		t.Fatal("brief execution contract lost")
	}
}

func launchCheckLine(t *testing.T, line launchLine, spec Spec, profile, reportsDir string) {
	t.Helper()
	if line.ID != "toy-"+spec.ID || line.Agent != spec.Agent || line.PlanTask != "Phase "+spec.ID {
		t.Fatalf("line: %+v", line)
	}
	if line.Profile != profile || line.Timeout != "1m0s" || line.Tools == nil || !*line.Tools {
		t.Fatalf("routing: %+v", line)
	}
	if !strings.Contains(line.Prompt, spec.Task) || !strings.Contains(line.Prompt, "Report: "+filepath.Join(reportsDir, line.ID+".md")) {
		t.Fatalf("brief: %s", line.Prompt)
	}
}

func TestLaunchRefusesBeforeChild(t *testing.T) {
	for _, slug := range []string{strings.Repeat("x", 61), "bad/slug", "bad slug"} {
		_, err := Launch(t.Context(), "/must-not-launch", []Spec{{ID: "1.1"}}, LaunchOptions{Slug: slug, ReportsDir: t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "batch id") {
			t.Fatalf("%q: %v", slug, err)
		}
	}
}

func TestLaunchBatchTags(t *testing.T) {
	source, err := parser.ParseFile(token.NewFileSet(), "../ask/batch.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	tags := map[string]bool{}
	ast.Inspect(source, func(node ast.Node) bool {
		spec, ok := node.(*ast.TypeSpec)
		if !ok || spec.Name.Name != "batchLine" {
			return true
		}
		for _, field := range spec.Type.(*ast.StructType).Fields.List {
			tag, err := strconv.Unquote(field.Tag.Value)
			if err != nil {
				t.Fatal(err)
			}
			tags[reflect.StructTag(tag).Get("json")] = true
		}
		return false
	})
	typ := reflect.TypeFor[launchLine]()
	for field := range typ.Fields() {
		tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if !tags[tag] {
			t.Errorf("batch parser does not accept %s", tag)
		}
	}
}

func TestLaunchEnvelopeValidation(t *testing.T) {
	options := LaunchOptions{Slug: "toy", ReportsDir: t.TempDir(), Provider: "databricks", Profiles: []string{"dev"}}
	specs := []Spec{{ID: "1.1"}}
	for _, data := range []string{"", "{", `{"task_id":"other","status":"ok"}`, `{"task_id":"toy-1.1","status":"ok"}`, `{"task_id":"toy-1.1","status":"running"}`} {
		if _, err := launchEnvelopes([]byte(data), specs, options); err == nil {
			t.Fatalf("accepted %q", data)
		}
	}
	data, err := launchLines(specs, options)
	if err != nil {
		t.Fatal(err)
	}
	var line launchLine
	if err := json.Unmarshal(data, &line); err != nil {
		t.Fatal(err)
	}
	if line.Profile != "databricks·dev" {
		t.Fatalf("provider routing: %q", line.Profile)
	}
}

func TestLaunchWidthCap(t *testing.T) {
	executable := launchExecutable(t)
	dir := t.TempDir()
	t.Setenv("WAVES_LAUNCH_STDIN", filepath.Join(dir, "stdin.jsonl"))
	t.Setenv("WAVES_LAUNCH_JOBS", "24")
	specs := make([]Spec, 25)
	for i := range specs {
		specs[i] = Spec{ID: fmt.Sprintf("1.%d", i+1), Task: "cap"}
	}
	envelopes, err := Launch(t.Context(), executable, specs, LaunchOptions{Slug: "cap", ReportsDir: dir})
	if len(envelopes) != len(specs) {
		t.Fatalf("width cap: %d envelopes, %v", len(envelopes), err)
	}
}

func TestLaunchTerminalStatuses(t *testing.T) {
	options := LaunchOptions{Slug: "toy", ReportsDir: t.TempDir()}
	for _, status := range []string{"error", "partial", "quota", "stalled"} {
		data := fmt.Sprintf(`{"task_id":"toy-1.1","status":%q}`, status)
		envelopes, err := launchEnvelopes([]byte(data), []Spec{{ID: "1.1"}}, options)
		if err != nil || envelopes[0].Status != status {
			t.Fatalf("%s: %+v, %v", status, envelopes, err)
		}
	}
}

func TestLaunchForwardsRouteFlags(t *testing.T) {
	executable := launchExecutable(t)
	dir := t.TempDir()
	t.Setenv("WAVES_LAUNCH_STDIN", filepath.Join(dir, "stdin.jsonl"))
	t.Setenv("WAVES_LAUNCH_JOBS", "1")
	route := []string{"--overlay", `{"fallback":"none"}`, "--mode", "plan", "--allow", "Bash(ls:*)", "--deny", "Edit"}
	t.Setenv("WAVES_LAUNCH_ROUTE", strings.Join(route, "\x1f"))
	options := LaunchOptions{Slug: "toy", ReportsDir: dir, Route: route}
	envelopes, err := Launch(t.Context(), executable, []Spec{{ID: "1.1", Agent: "codex", Task: "only"}}, options)
	if err != nil || len(envelopes) != 1 || envelopes[0].Status != "ok" {
		t.Fatalf("Launch with route flags: %+v, %v (a helper exit 5 means the flags did not reach the child)", envelopes, err)
	}
}
