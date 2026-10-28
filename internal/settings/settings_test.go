package settings

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hookspec"
	"github.com/edotau/claude-code/internal/paths"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestRenderGolden(t *testing.T) {
	got, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "settings.golden.json")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("render drifted from golden (go test -run TestRenderGolden -update):\n%s", got)
	}
	if !json.Valid(got) {
		t.Error("render is not valid JSON")
	}
}

const existing = `{
  "env": {"KEEP_ME": "1"},
  "companyAnnouncements": ["hi"],
  "permissions": {"allow": ["Bash(ls:*)"], "deny": ["Read(~/.ssh/**)"], "additionalDirectories": ["/x"]},
  "statusLine": {"type": "command", "command": "mine"},
  "hooks": {
    "Stop": [{"hooks": [
      {"type": "command", "command": "orca-hook && echo '<done>'", "timeout": 10},
      {"type": "command", "command": "$HOME/.claude/bin/claude-code hook retired-hook", "timeout": 5}
    ]}],
    "SessionEnd": [{"hooks": [{"type": "command", "command": "foreign-end"}]}]
  },
  "autoUpdates": false
}`

func TestMergePreservesForeign(t *testing.T) {
	rendered, _ := Render()
	merged, sum, err := Merge([]byte(existing), rendered, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Env         map[string]string           `json:"env"`
		Tips        *bool                       `json:"spinnerTipsEnabled"`
		Permissions map[string]json.RawMessage  `json:"permissions"`
		StatusLine  map[string]any              `json:"statusLine"`
		Hooks       map[string][]hookspec.Group `json:"hooks"`
		Output      string                      `json:"outputStyle"`
	}
	if err := json.Unmarshal(merged, &doc); err != nil {
		t.Fatalf("%v\n%s", err, merged)
	}
	if doc.Env["KEEP_ME"] != "1" || doc.Env["CLAUDE_CODE_SCROLL_SPEED"] == "" {
		t.Errorf("env not unioned: %v", doc.Env)
	}
	if doc.Tips == nil || *doc.Tips || doc.Output != "Concise" {
		t.Error("foreign top-level keys dropped")
	}
	if !strings.Contains(string(doc.Permissions["additionalDirectories"]), "/x") || !strings.Contains(string(doc.Permissions["allow"]), "Bash(ls:*)") {
		t.Errorf("permissions not merged: %s", merged)
	}
	if strings.Count(string(doc.Permissions["deny"]), "Read(~/.ssh/**)") != 1 {
		t.Error("deny list duplicated an existing rule")
	}
	cmds := map[string][]string{}
	for ev, groups := range doc.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				cmds[ev] = append(cmds[ev], h.Command)
			}
		}
	}
	stop := strings.Join(cmds["Stop"], "\n")
	if !strings.Contains(stop, "orca-hook && echo '<done>'") || strings.Contains(stop, "retired-hook") || !strings.Contains(stop, "hook stop") {
		t.Errorf("Stop hooks wrong: %q", cmds["Stop"])
	}
	// PreToolUse carries every registry PreToolUse spec: bash-guard, docs-placement, agent-inflight.
	wantPre := 0
	for _, s := range hookspec.Registry {
		if s.Event == hookspec.EventPreToolUse {
			wantPre++
		}
	}
	if len(cmds["SessionEnd"]) != 2 || cmds["SessionEnd"][1] != "foreign-end" || len(cmds["PreToolUse"]) != wantPre {
		t.Errorf("hooks: %v", cmds)
	}
	if strings.Contains(string(merged), "\\u0026") {
		t.Error("merge HTML-escaped a foreign command")
	}
	if sum.ForeignHooks != 2 || sum.HarnessHooks != len(hookspec.Registry) {
		t.Errorf("summary %+v", sum)
	}
	if !strings.HasPrefix(strings.Join(sum.Kept, ","), "companyAnnouncements,autoUpdates") {
		t.Errorf("kept %v", sum.Kept)
	}
	again, _, err := Merge(merged, rendered, nil)
	if err != nil || string(again) != string(merged) {
		t.Errorf("merge is not idempotent:\n%s", again)
	}
}

// hermeticCacheDir isolates CLAUDE_CONFIG_DIR so WriteWithBackup's paths.BackupPath never touches
// the real ~/.claude/state/harness.
func hermeticCacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	return dir
}

func TestMergeEmptyAndBackup(t *testing.T) {
	hermeticCacheDir(t)
	rendered, _ := Render()
	merged, _, err := Merge(nil, rendered, nil)
	if err != nil || string(merged) != string(rendered) {
		t.Fatalf("merge onto nothing should equal the render: %v", err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "settings.json")
	if b, err := WriteWithBackup(file, []byte("{}\n")); err != nil || b != "" {
		t.Fatalf("fresh write: %q %v", b, err)
	}
	beforeSiblings, _ := os.ReadDir(dir)
	b, err := WriteWithBackup(file, merged)
	if err != nil || b == "" {
		t.Fatalf("backup: %q %v", b, err)
	}
	if old, _ := os.ReadFile(b); string(old) != "{}\n" {
		t.Errorf("backup content %q", old)
	}
	if !strings.HasPrefix(b, filepath.Join(paths.CacheDir(), "backups")) {
		t.Errorf("backup %q, want it under CacheDir()/backups", b)
	}
	afterSiblings, _ := os.ReadDir(dir)
	if len(afterSiblings) != len(beforeSiblings) {
		t.Errorf("files beside dest changed: before %v, after %v", beforeSiblings, afterSiblings)
	}
}

func TestWriteWithBackupPreservesBackToBackBackups(t *testing.T) {
	hermeticCacheDir(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "settings.json")
	original := []byte("{\"original\":true}\n")
	if err := os.WriteFile(file, original, 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := WriteWithBackup(file, []byte("{\"updated\":true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := WriteWithBackup(file, []byte("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Errorf("back-to-back writes reused backup path %q", first)
	}
	got, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("first backup = %q, want %q", got, original)
	}
	for _, b := range []string{first, second} {
		if !strings.HasPrefix(b, filepath.Join(paths.CacheDir(), "backups")) {
			t.Errorf("backup %q, want it under CacheDir()/backups", b)
		}
	}
	siblings, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(siblings) != 1 {
		t.Errorf("files beside dest = %v, want only settings.json itself", siblings)
	}
}

func TestMergeDropsRetiredKeys(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	in := `{"companyAnnouncements":["hi"],"autoMemoryDirectory":"/flat/memory","zzForeign":{"a":1}}`
	out, sum, err := Merge([]byte(in), rendered, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "autoMemoryDirectory") {
		t.Errorf("retired key survived:\n%s", out)
	}
	if strings.Join(sum.Retired, ",") != "autoMemoryDirectory" {
		t.Errorf("retired = %v", sum.Retired)
	}
	if strings.Join(sum.Kept, ",") != "companyAnnouncements,zzForeign" {
		t.Errorf("kept = %v (want foreign keys in order, no retired key)", sum.Kept)
	}
	if i, j := strings.Index(string(out), "companyAnnouncements"), strings.Index(string(out), "zzForeign"); i < 0 || j < i {
		t.Errorf("foreign key order lost:\n%s", out)
	}
	again, sum2, err := Merge(out, rendered, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(out) {
		t.Error("merge is not idempotent")
	}
	if len(sum2.Retired) != 0 {
		t.Errorf("second merge retired %v", sum2.Retired)
	}
}

func TestMergeDropsRetiredEnvKeys(t *testing.T) {
	in := `{"env":{"CLAUDE_CODE_DISABLE_VIRTUAL_SCROLL":"1","KEEP":"1"}}`
	out, sum, err := Merge([]byte(in), []byte(`{"env":{"A":"b"}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); strings.Contains(s, "VIRTUAL_SCROLL") || !strings.Contains(s, `"KEEP"`) {
		t.Errorf("merged env = %s", s)
	}
	if !slices.Contains(sum.Retired, "env.CLAUDE_CODE_DISABLE_VIRTUAL_SCROLL") {
		t.Errorf("retired = %v", sum.Retired)
	}
}

func TestMergeHooksPreservesForeignFieldsAndOrder(t *testing.T) {
	base := `{"Stop":[{"matcher":"x","extra":{"keep":true},"hooks":[
		{"custom":1,"type":"command","command":"first","tail":false},
		{"command":"$HOME/.claude/bin/claude-code hook retired-hook"},
		{"type":"prompt","prompt":"keep this","custom":2}
	],"timeout":9},{"hooks":[{"command":"last"}],"tail":"end"}]}`
	want := `{"Stop":[{"matcher":"x","extra":{"keep":true},"hooks":[{"custom":1,"type":"command","command":"first","tail":false},{"type":"prompt","prompt":"keep this","custom":2}],"timeout":9},{"hooks":[{"command":"last"}],"tail":"end"}]}`
	var sum Summary
	got, err := mergeHooks(json.RawMessage(base), json.RawMessage(`{}`), &sum)
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, got); err != nil {
		t.Fatal(err)
	}
	if compact.String() != want || sum.ForeignHooks != 3 || sum.HarnessHooks != 0 {
		t.Fatalf("mergeHooks = %s, %+v", got, sum)
	}
}

// TestMergeDropsListedForeignKey: a dropped foreign key is removed and recorded; a template key in the list stays.
func TestMergeDropsListedForeignKey(t *testing.T) {
	rendered, err := Render()
	if err != nil {
		t.Fatal(err)
	}
	in := `{"skipDangerousModePermissionPrompt":true,"keepMe":1,"env":{"A":"b"}}`
	out, sum, err := Merge([]byte(in), rendered, []string{"skipDangerousModePermissionPrompt", "env"})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["skipDangerousModePermissionPrompt"]; ok {
		t.Error("dropped foreign key survived Merge")
	}
	if _, ok := doc["keepMe"]; !ok {
		t.Error("unlisted foreign key was removed")
	}
	if _, ok := doc["env"]; !ok {
		t.Error("template-owned key in the drop list must still be rendered")
	}
	if !slices.Contains(sum.Dropped, "skipDangerousModePermissionPrompt") {
		t.Errorf("Summary.Dropped = %v, want it to list the foreign key", sum.Dropped)
	}
}

func TestMergeKeepsRuntimeKeysWithoutReportingThem(t *testing.T) {
	rendered, _ := Render()
	live := `{"modelSettings":{"claude-fable-5-1":{"effortLevel":"high"}},"someForeign":1}`
	merged, sum, err := Merge([]byte(live), rendered, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(merged), `"effortLevel": "high"`) && !strings.Contains(string(merged), `"effortLevel":"high"`) {
		t.Errorf("modelSettings not kept:\n%s", merged)
	}
	if slices.Contains(sum.Kept, "modelSettings") {
		t.Errorf("runtime key reported as foreign: %v", sum.Kept)
	}
	if !slices.Contains(sum.Kept, "someForeign") {
		t.Errorf("a real foreign key must still be reported: %v", sum.Kept)
	}
}
