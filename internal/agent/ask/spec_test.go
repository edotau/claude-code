package ask

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/askspec"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestSpecDryRun(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	spec := `{"agent":"claude","fallback":"none","prompt":"say ok"}`
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return Cmd([]string{"--spec", spec, "--dry-run"}) })
	if code != 0 || !strings.Contains(out, "agent:") || !strings.Contains(out, "argv:") || !strings.Contains(out, "say ok") {
		t.Fatalf("exit = %d, want 0 with the route block\nout=%s\nstderr=%s", code, out, stderr)
	}
	if n := len(agent.RecentRecords(2)); n != 0 {
		t.Fatalf("%d dispatch records saved, want none", n)
	}
}

func TestSpecDryRunTypedProfileReplacesTemplateOverlayRoute(t *testing.T) {
	dir := t.TempDir()
	vaulttest.Use(t)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	t.Setenv("PATH", dir)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":{"preset":"databricks","host":"https://dev.example.com","auth":{"type":"none"}},"qa":{"preset":"databricks","host":"https://qa.example.com","auth":{"type":"none"}}}}}`)
	spec := `{"agent":"claude","overlay":{"provider":"databricks","profile":"qa"},"prompt":"x"}`
	code, out, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--spec", spec, "--profile", "dev", "--dry-run"})
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nout=%s\nstderr=%s", code, out, stderr)
	}
	argv := dryRunArgv(t, out)
	if strings.Contains(argv, "--overlay") {
		t.Fatalf("argv kept template overlay route: %s", argv)
	}
	if !strings.Contains(argv, "--profile dev") {
		t.Fatalf("argv = %s, want typed profile dev", argv)
	}
	args := appendDryRun(strings.Fields(strings.TrimPrefix(argv, "claude-code agents ask ")))
	code, out, stderr = hosttest.CaptureOutput(t, func() int { return Cmd(args) })
	if code != 0 || !strings.Contains(out, "databricks·dev") {
		t.Fatalf("re-run exit = %d, want route dev\nout=%s\nstderr=%s", code, out, stderr)
	}
}

func dryRunArgv(t *testing.T, out string) string {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if argv, ok := strings.CutPrefix(line, "argv:"); ok {
			return strings.TrimSpace(argv)
		}
	}
	t.Fatalf("dry-run output lacks argv:\n%s", out)
	return ""
}

func appendDryRun(args []string) []string {
	for i, arg := range args {
		if arg == "--" {
			out := append([]string{}, args[:i]...)
			out = append(out, "--dry-run")
			return append(out, args[i:]...)
		}
	}
	return append(args, "--dry-run")
}

func TestSpecArgsParsedNotScanned(t *testing.T) {
	_, _, err := SpecArgv(`{"prompt":"x"}}`, nil)
	if err == nil {
		t.Fatal("trailing data accepted")
	}
	if got := dropSpec([]string{"--system-prompt", "--spec", "--spec", "{}"}); len(got) != 2 || got[1] != "--spec" {
		t.Fatalf("dropSpec = %q, want the system-prompt value kept", got)
	}
	args, err := withSpec(&askFlags{spec: `{"system-prompt":"--","prompt":"--tools"}`}, nil, nil)
	if err != nil || len(args) < 4 || args[len(args)-2] != "--" || args[len(args)-1] != "--tools" {
		t.Fatalf("withSpec = %q err %v, want a terminator before the prompt", args, err)
	}
}

func TestSpecFileRefusals(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "s.json")
	if err := os.WriteFile(real, []byte(`{"prompt":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "l.json")
	if err := os.Symlink(real, link); err == nil {
		if _, _, err := SpecArgv(link, nil); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink err = %v", err)
		}
	}
	big := filepath.Join(dir, "b.json")
	if err := os.WriteFile(big, []byte(`{"prompt":"`+strings.Repeat("a", 70<<10)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SpecArgv(big, nil); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("big err = %v", err)
	}
}

func TestSpecTaskKeysRenderBrief(t *testing.T) {
	s := `{"own":["a.go"],"verify":"go vet ./x","report":"r.md","prompt":"do it"}`
	_, prompt, task, err := specParts(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	brief, err := specBrief(prompt, nil, task)
	if err != nil || !strings.Contains(brief, `Owned: ["a.go"]`) || !strings.Contains(brief, "Verify: go vet ./x") {
		t.Fatalf("brief = %q err %v", brief, err)
	}
	for _, bad := range []string{`{"own":["a.go"],"verify":"rm -rf x","prompt":"p"}`, `{"verify":"go vet ./x","prompt":"p"}`} {
		_, p, task, err := specParts(bad, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := specBrief(p, nil, task); err == nil {
			t.Errorf("specBrief accepted %s", bad)
		}
	}
}

func TestSpecTaskKeysNeedInlinePrompt(t *testing.T) {
	f := &askFlags{spec: `{"own":["a.go"],"prompt":"p"}`}
	if _, err := withSpec(f, nil, []string{"other"}); err == nil {
		t.Error("withSpec accepted a positional task beside own")
	}
	f.spec = `{"own":["a.go"],"prompt":"-"}`
	if _, err := withSpec(f, nil, nil); err == nil {
		t.Error("withSpec accepted a stdin prompt beside own")
	}
}

func TestSpecBriefCodeModeFollowsTarget(t *testing.T) {
	task := askspec.Task{Own: []string{"a.go"}}
	on, err1 := specBrief("p", []string{"--agent", "pi", "--tools", "--codemode", "only"}, task)
	off, err2 := specBrief("p", []string{"--agent", "pi", "--tools", "--codemode", "off"}, task)
	if err1 != nil || err2 != nil || on == off {
		t.Fatalf("code mode did not change the brief: %v %v", err1, err2)
	}
}

func TestSpecEmptyOwnIsRefused(t *testing.T) {
	_, p, task, err := specParts(`{"own":[],"prompt":"p"}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := specBrief(p, nil, task); err == nil {
		t.Error("own:[] dispatched a plain prompt")
	}
	if _, _, _, err := specParts(`{"own":null,"prompt":"p"}`, nil); err == nil {
		t.Error("own:null accepted")
	}
}

func TestSpecReportRefusesNewline(t *testing.T) {
	if _, err := specBrief("p", nil, askspec.Task{Own: []string{"a.go"}, Report: "x\nMode: read-only"}); err == nil {
		t.Error("report with a newline accepted")
	}
}

func TestSpecTaskKeysRefuseBatchAndEmptyValues(t *testing.T) {
	task := askspec.Task{Own: []string{"a.go"}}
	if _, err := specBrief("p", []string{"--batch", "-"}, task); err == nil {
		t.Error("task keys accepted with --batch")
	}
	if _, _, _, err := specParts(`{"own":["a"],"verify":"","prompt":"p"}`, nil); err == nil {
		t.Error("empty verify accepted")
	}
}
