package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
)

// SIGTERM mid-run (a dead-proxy hang killed by `timeout` or a tool) finalizes the dispatch record, never `running`.
func TestAskSIGTERMFinalizesRecord(t *testing.T) {
	if os.Getenv("CC_ASK_SIGTERM_CHILD") == "1" {
		os.Exit(cmdAsk([]string{"--agent", "claude", "--fallback", "none", "--format", "json", "say hi"}))
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	marker := filepath.Join(t.TempDir(), "started")
	if err := os.MkdirAll(paths.BinDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\ntouch '" + marker + "'\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(paths.BinDir(), "claude"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestAskSIGTERMFinalizesRecord$")
	cmd.Env = append(os.Environ(), "CC_ASK_SIGTERM_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("fake claude never started")
		}
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	files, _ := filepath.Glob(filepath.Join(agent.DispatchDir(), "*.json"))
	if len(files) != 1 {
		t.Fatalf("want one dispatch record, got %q", files)
	}
	var rec agent.Record
	b, _ := os.ReadFile(files[0])
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Status != "error" {
		t.Fatalf("record status = %q after SIGTERM, want error", rec.Status)
	}
}
