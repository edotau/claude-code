package audit

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLogReadRoundTripAndDropsInvalid(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	Log(ProviderSwitch, "gemini", "")
	Log(Action("bogus"), "x", "")
	Log(HookBlocked, "safety", "destructive command")
	ev, err := Read(time.Time{})
	if err != nil || len(ev) != 2 || ev[0].Action != ProviderSwitch || ev[1].Detail != "destructive command" {
		t.Fatalf("Read = %+v, %v", ev, err)
	}
	if s := Summary(ev); s[ProviderSwitch] != 1 || s[HookBlocked] != 1 {
		t.Errorf("Summary = %v", s)
	}
	if fi, _ := os.Stat(File()); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// A credential in any field is replaced before the line reaches disk.
func TestLogRedactsCredentials(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tok := "sk" + "-abcdefghijklmnopqrstuvwxyz0123"
	Log(SecretSet, "OPENAI_API_KEY", "value "+tok)
	b, _ := os.ReadFile(File())
	if strings.Contains(string(b), tok) || !strings.Contains(string(b), "REDACTED") {
		t.Errorf("log line: %s", b)
	}
}

// Past the cap the live file rotates to .1, and Read still sees both, skipping a malformed line.
func TestRotationAndMalformedLines(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := maxBytes
	maxBytes = 200
	defer func() { maxBytes = old }()
	for range 5 {
		Log(ModelPin, "HARNESS_OPUS_MODEL", "claude-opus-5-5")
	}
	if _, err := os.Stat(File() + ".1"); err != nil {
		t.Fatalf("no rotation: %v", err)
	}
	f, _ := os.OpenFile(File(), os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("not json\n")
	f.Close()
	ev, _ := Read(time.Time{})
	if len(ev) < 2 {
		t.Errorf("Read across rotation = %d events", len(ev))
	}
}
