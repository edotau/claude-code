package ask

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
)

// attachFlag.Set resolves a real file to an absolute path and errors on anything else: missing,
// directory, or a path flag.Abs itself cannot resolve.
func TestAttachFlagSet(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	writeFixtureFile(t, f, "hi")

	var a attachFlag
	if err := a.Set(f); err != nil {
		t.Fatalf("Set(%q): %v", f, err)
	}
	if len(a) != 1 || a[0] != f {
		t.Errorf("a = %q, want [%q]", a, f)
	}

	if err := a.Set(filepath.Join(dir, "missing.txt")); err == nil {
		t.Error("Set on a missing path: want an error, got nil")
	}
	if err := a.Set(dir); err == nil {
		t.Error("Set on a directory: want an error, got nil")
	}
}

// keepEnvFlag.Set comma-splits each occurrence and accumulates across repeats.
func TestKeepEnvFlagSet(t *testing.T) {
	var k keepEnvFlag
	if err := k.Set("DATABRICKS_HOST,DATABRICKS_TOKEN"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := k.Set("HOME"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !slices.Equal([]string(k), []string{"DATABRICKS_HOST", "DATABRICKS_TOKEN", "HOME"}) {
		t.Errorf("k = %q, want accumulated comma-split names", k)
	}
}

// attachFlag.Set rejects binary files up front (NUL, invalid UTF-8) and accepts multibyte UTF-8 text.
func TestAttachFlagSetRejectsBinary(t *testing.T) {
	dir := t.TempDir()
	var a attachFlag
	for name, data := range map[string]string{"nul.bin": "ab\x00cd", "bad.dat": "\xff\xfe\x80"} {
		f := filepath.Join(dir, name)
		writeFixtureFile(t, f, data)
		err := a.Set(f)
		if err == nil || !strings.Contains(err.Error(), "binary file") || !strings.Contains(err.Error(), name) {
			t.Errorf("Set(%s): err = %v, want a binary-file error naming the path", name, err)
		}
	}
	if len(a) != 0 {
		t.Errorf("a = %q, want nothing recorded for rejected files", a)
	}
	f := filepath.Join(dir, "utf8.txt")
	writeFixtureFile(t, f, "héllo 世界")
	if err := a.Set(f); err != nil || len(a) != 1 {
		t.Errorf("Set(multibyte text): err = %v, a = %q, want accepted", err, a)
	}
}

const acmeProvider = `"providers":{"openai":{"acme":{"protocol":"openai","base_url":"https://acme.example/v1","auth":{"type":"none"}}}}`

// profileFixture isolates the vault and seeds provider acme (no opencodeID) plus, when active is set, a "work" set.
func profileFixture(t *testing.T, active string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	doc := `{` + acmeProvider + `}`
	if active != "" {
		doc = `{` + acmeProvider + `,"profiles":{"active":"` + active + `","sets":{"work":{
		  "codex":{"account":"acme","model":"acme-large","effort":"high"}}}}}`
	}
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, "providers", doc) }); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}
}

func TestClientPrepareHubNativeFallback(t *testing.T) {
	profileFixture(t, "")
	t.Setenv("HOME", t.TempDir())
	pathstest.FakeVendor(t, "opencode", `printf '%s\n' '{"type":"text","part":{"text":"ok"}}'`)
	client := NewClient()
	for _, profile := range []string{"acme", "chatgpt"} {
		p, err := client.Prepare(context.Background(), agent.RunOptions{
			Agent: "opencode", Provider: profile, Prompt: "p", Fallback: router.FallbackAgents}, nil)
		if err != nil {
			t.Fatalf("Prepare(%s) = %v", profile, err)
		}
		res, err := client.Exec(context.Background(), p, nil, job.RunRecordedOpts{})
		if len(res.Hops) != 1 {
			t.Fatalf("%s: want one dispatch, got %+v, err %v", profile, res.Hops, err)
		}
		if profile == "acme" && (err != nil || res.Answer != "ok") {
			t.Fatalf("native profile did not run: %+v, %v", res, err)
		}
		if profile == "chatgpt" && (err == nil || !strings.Contains(err.Error(), "opencode has no openai login")) {
			t.Fatalf("hub profile did not check its login: %+v, %v", res, err)
		}
	}
}

// A cancelled batch prepares nothing and never refreshes quota.
func TestClientPrepareHonorsCancellation(t *testing.T) {
	refreshes := 0
	prev := RouteRefresh
	RouteRefresh = func(context.Context, string) bool { refreshes++; return false }
	t.Cleanup(func() { RouteRefresh = prev })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewClient().Prepare(ctx, agent.RunOptions{Agent: "opencode", Prompt: "p", Fallback: router.FallbackAgents}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if refreshes != 0 {
		t.Errorf("%d quota refreshes ran under a cancelled ctx", refreshes)
	}
}

// An explicit --profile with no --model runs the classifier's alias, as the pinned route does.
func TestClientPrepareProfileTakesTierSlot(t *testing.T) {
	profileFixture(t, "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	client := NewClient()
	for _, profile := range []string{"chatgpt", "acme"} {
		p, err := client.Prepare(context.Background(), agent.RunOptions{
			Agent: "opencode", Provider: profile, Prompt: "pong", Fallback: router.FallbackAgents}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, _ := client.Exec(context.Background(), p, nil, job.RunRecordedOpts{})
		if len(res.Hops) != 1 {
			t.Fatalf("%s: hops = %+v, want one unavailable dispatch", profile, res.Hops)
		}
		if want := (router.RuleClassifier{}).Alias("pong"); res.Hops[0].Model != want {
			t.Errorf("%s: dispatch model = %q, want alias %q", profile, res.Hops[0].Model, want)
		}
	}
}

// An explicit --profile is one dispatch for every agent even with --fallback all: nothing can silently take over.
func TestClientPrepareProfileIsOneDispatchUnderFallbackAll(t *testing.T) {
	profileFixture(t, "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	client := NewClient()
	for _, name := range []string{"codex", "claude", "opencode", "copilot", "gemini"} {
		p, err := client.Prepare(context.Background(), agent.RunOptions{
			Agent: name, Provider: "acme", Prompt: "p", Fallback: router.FallbackAll}, nil)
		if err != nil {
			t.Fatalf("%s: Prepare = %v", name, err)
		}
		res, err := client.Exec(context.Background(), p, nil, job.RunRecordedOpts{})
		if err == nil || len(res.Hops) != 1 {
			t.Errorf("%s: %d hops, err %v; want one failed dispatch without fallback", name, len(res.Hops), err)
		}
	}
}

// writeFixtureFile creates path (with parents) holding content.
func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// A background start whose job-id line cannot be written reports the write error instead of exiting 0 silently.
func TestReportBackgroundStartReturnsWriteError(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close() // a reader-less pipe fails every write with EPIPE
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig; w.Close() })
	for _, format := range []string{helptext.Text, helptext.JSON} {
		if err := ReportBackgroundStart(format, job.Record{ID: "j1"}); err == nil {
			t.Errorf("%s: write to a closed pipe returned nil", format)
		}
	}
}
