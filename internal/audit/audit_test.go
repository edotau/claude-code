package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
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
	if fi, _ := os.Stat(logFile()); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// A failed credential seal is a valid action and keeps the vault secret's name and the kind readable.
func TestSealFailedIsLoggedWithNameAndKind(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	Log(SealFailed, "DATABRICKS_DEV_OAUTH", "refresh")
	ev, err := Read(time.Time{})
	if err != nil || len(ev) != 1 || ev[0].Action != SealFailed || ev[0].Resource != "DATABRICKS_DEV_OAUTH" || ev[0].Detail != "refresh" {
		t.Fatalf("Read = %+v, %v; want one seal_failed event naming the secret and kind", ev, err)
	}
}

// A credential in any field is replaced before the line reaches disk.
func TestLogRedactsCredentials(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	tok := "sk" + "-abcdefghijklmnopqrstuvwxyz0123"
	Log(SecretSet, "OPENAI_API_KEY", "value "+tok)
	b, _ := os.ReadFile(logFile())
	if strings.Contains(string(b), tok) || !strings.Contains(string(b), "REDACTED") {
		t.Errorf("log line: %s", b)
	}
}

// A credential embedded inside a KEY=value detail (not just a whole field) is still redacted.
func TestLogRedactsEmbeddedCredential(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	Log(SecretSet, "AWS_SECRET", "export AWS_SECRET=AKIAIOSFODNN7EXAMPLE")
	b, _ := os.ReadFile(logFile())
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
	if _, err := os.Stat(logFile() + ".1"); err != nil {
		t.Fatalf("no rotation: %v", err)
	}
	f, err := os.OpenFile(logFile(), os.O_APPEND|os.O_WRONLY, 0o600)
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
	if err := os.MkdirAll(filepath.Dir(logFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logFile(), []byte(seed), 0o600); err != nil {
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
	b, _ := os.ReadFile(logFile())
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, `"schema":1`) {
		t.Errorf("new line lacks schema 1: %s", last)
	}
}

// TestRotateTwiceKeepsRotatedHistory pins: a second writer that saw the full log rotates once, not over .1.
func TestRotateTwiceKeepsRotatedHistory(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := maxBytes
	maxBytes = 50
	defer func() { maxBytes = old }()
	file := logFile()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	full := strings.Repeat("x", 100)
	if err := os.WriteFile(file, []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	rotate(file)
	if err := os.WriteFile(file, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rotate(file)
	got, err := os.ReadFile(file + ".1")
	if err != nil || string(got) != full {
		t.Errorf(".1 = %q, %v; want the original full log", got, err)
	}
}

// TestRotateKeepsPreviousGeneration pins: a second rotation moves the old .1 to .2 instead of overwriting it.
func TestRotateKeepsPreviousGeneration(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := maxBytes
	maxBytes = 50
	defer func() { maxBytes = old }()
	file := logFile()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	first, second := strings.Repeat("a", 100), strings.Repeat("b", 100)
	for _, gen := range []string{first, second} {
		if err := os.WriteFile(file, []byte(gen), 0o600); err != nil {
			t.Fatal(err)
		}
		rotate(file)
	}
	if got, _ := os.ReadFile(file + ".1"); string(got) != second {
		t.Errorf(".1 = %q, want the newest generation", got)
	}
	if got, _ := os.ReadFile(file + ".2"); string(got) != first {
		t.Errorf(".2 = %q, want the first generation preserved", got)
	}
}

func TestReadIncludesEveryKeptArchive(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(logFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	suffixes := []string{".5", ".4", ".3", ".2", ".1", ""}
	for _, suffix := range suffixes {
		seed := `{"schema":1,"time":"2026-11-08T23:59:59Z","action":"model_pin","resource":"expired"}` + "\n" +
			fmt.Sprintf(`{"schema":1,"time":"2026-11-09T00:00:00Z","action":"model_pin","resource":%q}`, suffix) + "\n"
		if err := os.WriteFile(logFile()+suffix, []byte(seed), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	events, err := Read(time.Date(2026, time.November, 9, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var resources []string
	for _, event := range events {
		resources = append(resources, event.Resource)
	}
	if got, want := strings.Join(resources, ","), strings.Join(suffixes, ","); got != want {
		t.Errorf("Read resources = %q, want %q in archive order", got, want)
	}
	if got := Summary(events)[ModelPin]; got != len(suffixes) {
		t.Errorf("Summary model_pin = %d, want %d", got, len(suffixes))
	}
}

func TestRotatePreservesHistoryOnFailure(t *testing.T) {
	for _, suffix := range []string{".5", ".lock"} {
		t.Run(suffix, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "audit.jsonl")
			live := strings.Repeat("x", int(maxBytes)+1)
			for name, data := range map[string]string{file: live, file + ".1": "previous generation", file + ".4": "older"} {
				if err := os.WriteFile(name, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(file+suffix, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := rotate(file); err == nil {
				t.Error("rotate succeeded despite blocked " + suffix)
			}
			for path, want := range map[string]string{file: live, file + ".1": "previous generation"} {
				if got, err := os.ReadFile(path); err != nil || string(got) != want {
					t.Errorf("rotation did not preserve %s: bytes = %d, error = %v", path, len(got), err)
				}
			}
		})
	}
}

// A pre-existing loose state dir and audit file are tightened, not left at their old modes.
func TestLogTightensExistingModes(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	file := logFile()
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	Log(ProviderSwitch, "gemini", "")
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestMain(m *testing.M) { os.Exit(hosttest.Main(m)) }
