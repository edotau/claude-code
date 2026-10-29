package audit

import (
	"os"
	"path/filepath"
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

// A credential embedded inside a KEY=value detail (not just a whole field) is still redacted.
func TestLogRedactsEmbeddedCredential(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	Log(SecretSet, "AWS_SECRET", "export AWS_SECRET=AKIAIOSFODNN7EXAMPLE")
	b, _ := os.ReadFile(File())
	if strings.Contains(string(b), "AKIAIOSFODNN7EXAMPLE") || !strings.Contains(string(b), "REDACTED") {
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
	f, err := os.OpenFile(File(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString("not json\n"); err != nil {
		t.Fatal(err)
	}
	ev, _ := Read(time.Time{})
	if len(ev) < 2 {
		t.Errorf("Read across rotation = %d events", len(ev))
	}
}

// TestAuditLogSchemaStampsNewLinesAndSkipsNewer pins: unstamped lines read, new lines carry schema 1, schema-2 lines are skipped.
func TestAuditLogSchemaStampsNewLinesAndSkipsNewer(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ts := time.Now().UTC().Format(time.RFC3339)
	seed := `{"time":"` + ts + `","action":"model_pin","resource":"old"}` + "\n" +
		`{"schema":2,"time":"` + ts + `","action":"model_pin","resource":"future"}` + "\n"
	if err := os.MkdirAll(filepath.Dir(File()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(File(), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	Log(KeyRotate, "new", "")

	events, err := Read(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var res []string
	for _, e := range events {
		res = append(res, e.Resource)
	}
	if strings.Join(res, ",") != "old,new" {
		t.Errorf("resources = %v, want [old new] (schema 2 skipped)", res)
	}
	b, _ := os.ReadFile(File())
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, `"schema":1`) {
		t.Errorf("new line lacks schema 1: %s", last)
	}
}
