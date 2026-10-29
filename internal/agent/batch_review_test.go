package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func hmJWT(iss string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"iss":"https://`+iss+`"}`)) + ".sig"
}

// hmProfile overlays a Databricks profile "hm" on host adb-1 whose credential comes from auth.command cmd.
func hmProfile(t *testing.T, cmd string) {
	t.Helper()
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"hm":{"preset":"databricks",`+
		`"host":"https://adb-1.azuredatabricks.net","auth":{"type":"key","command":`+fmt.Sprintf("%q", cmd)+`},`+
		`"models":{"opus":"databricks-claude-opus-5-5","sonnet":"databricks-claude-sonnet-5-5","haiku":"databricks-claude-haiku-4-5"}}}}}`)
}

// A line with trailing junk after its object is that line's error; Decoder.More alone accepts a stray '}'.
func TestReadTaskLinesRejectsTrailingJunk(t *testing.T) {
	in := "{\"prompt\":\"x\"}}\n{\"prompt\":\"a\"} {\"prompt\":\"b\"}\n{\"prompt\":\"ok\"}\n"
	got, err := ReadTaskLines(strings.NewReader(in))
	if err != nil || len(got) != 3 {
		t.Fatalf("got %d lines, err %v", len(got), err)
	}
	if got[0].Err == nil || got[1].Err == nil || got[2].Err != nil {
		t.Errorf("errs = %v / %v / %v, want error, error, nil", got[0].Err, got[1].Err, got[2].Err)
	}
}

// A waiter whose cooldown is extended by another task while it sleeps must keep waiting for the new deadline.
func TestWaitCooldownHonorsExtendedDeadline(t *testing.T) {
	key := "cool-extend-" + t.Name()
	coolDown(key, 100*time.Millisecond)
	start := time.Now()
	done := make(chan time.Duration)
	go func() { waitCooldown(context.Background(), key, 5*time.Second); done <- time.Since(start) }()
	time.Sleep(40 * time.Millisecond)
	coolDown(key, 300*time.Millisecond)
	if got := <-done; got < 300*time.Millisecond {
		t.Errorf("waiter resumed after %v, want it to wait out the extended ~340ms hold", got)
	}
	cooldowns.Lock()
	delete(cooldowns.until, key)
	cooldowns.Unlock()
}

type gaugeRunner struct {
	name     string
	cur, max *atomic.Int32
}

func (g gaugeRunner) Name() string     { return g.name }
func (g gaugeRunner) Caps() Caps       { return Caps{} }
func (g gaugeRunner) Available() error { return nil }
func (g gaugeRunner) Run(context.Context, Request, io.Writer) (Result, error) {
	n := g.cur.Add(1)
	for m := g.max.Load(); n > m && !g.max.CompareAndSwap(m, n); m = g.max.Load() {
	}
	time.Sleep(60 * time.Millisecond)
	g.cur.Add(-1)
	return Result{Answer: "a"}, nil
}

// Tasks queued under different agent names but legs billed to one account never run more than DefaultPerKey at once.
func TestBatchCapsConcurrencyPerBilledAccount(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var cur, peak atomic.Int32
	var tasks []BatchTask
	for i := range DefaultPerKey + 3 {
		name := fmt.Sprintf("agent%d", i)
		r := gaugeRunner{name: name, cur: &cur, max: &peak}
		tasks = append(tasks, BatchTask{ID: name, Agent: name, Req: Request{Prompt: "p"},
			Prepare: func(context.Context) (Chain, Record, error) {
				return Chain{Want: name, Legs: []Leg{{Agent: r, Account: "shared·acct", keepCaller: true}}}, Record{ID: "id-" + name, Agent: name, Log: filepath.Join(t.TempDir(), "l.log")}, nil
			}})
	}
	var out strings.Builder
	if !RunBatch(context.Background(), tasks, BatchOptions{Jobs: len(tasks), Out: &out}) {
		t.Fatalf("batch failed: %s", out.String())
	}
	if got := int(peak.Load()); got > DefaultPerKey || got < 2 {
		t.Errorf("peak concurrent legs on one account = %d, want 2..%d", got, DefaultPerKey)
	}
}

// A routed provider:model request on a vendor agent is guarded as that provider, not skipped as an own login.
func TestGuardProviderResolvesModelPrefixForVendorAgent(t *testing.T) {
	hmProfile(t, "true")
	p, err := guardProvider(&fakeRunner{name: "codex"}, Request{Model: "hm:opus"})
	if err != nil || p == nil || p.Name != "hm" {
		t.Fatalf("guardProvider = %v, %v; want the hm profile", p, err)
	}
}

// The host mismatch of a prefix-routed vendor request fails as auth and the runner never starts.
func TestRunPrefixRoutedHostMismatchFailsAuth(t *testing.T) {
	hmProfile(t, "true")
	t.Setenv("HM_KEY", hmJWT("adb-2.azuredatabricks.net"))
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"hm":{"preset":"databricks",`+
		`"host":"https://adb-1.azuredatabricks.net","auth":{"type":"key","env":"HM_KEY"},"models":{"opus":"m"}}}}}`)
	r := &fakeRunner{name: "codex", caps: Caps{Model: true}, errs: []error{nil}}
	_, err := Run(withIsolation(context.Background()), r, Request{Prompt: "p", Model: "hm:opus"}, io.Discard, nil)
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindAuth || len(r.seen) != 0 {
		t.Fatalf("err = %v, runs = %d; want KindAuth before any run", err, len(r.seen))
	}
}

// A transient credential-command failure is retried inside Run instead of failing the hop as auth.
func TestRunTransientCredentialFailureIsRetried(t *testing.T) {
	fastDefaultBackoff(t)
	flag := filepath.Join(t.TempDir(), "seen")
	hmProfile(t, fmt.Sprintf("if [ -f %[1]s ]; then echo %[2]s; else touch %[1]s; echo 'connection reset' >&2; exit 1; fi", flag, hmJWT("adb-1.azuredatabricks.net")))
	r := &fakeRunner{name: "claude", caps: Caps{Provider: true}, errs: []error{nil}}
	res, err := Run(context.Background(), r, Request{Prompt: "p", Provider: "hm"}, io.Discard, nil)
	if err != nil || res.Answer != "a" || res.Transport.Attempts != 2 {
		t.Fatalf("res=%+v err=%v; want an answer on attempt 2", res, err)
	}
}

// An isolated copilot SDK run blanks other profiles' credential env, since the SDK merges os.Environ itself.
func TestCopilotSDKEnvBlanksForeignCredentials(t *testing.T) {
	ghLogin(t, "alice", 10)
	fakeCopilotBin(t)
	pinGitHubProfile(t, "alice", `"anthropic":{"other":{"protocol":"anthropic","base_url":"http://127.0.0.1:1",`+
		`"auth":{"type":"key","env":"HM_OTHER_KEY"},"models":{"opus":"m"}}}`)
	t.Setenv("HM_OTHER_KEY", "foreign-secret")
	var mu sync.Mutex
	var seen []string
	prev := copilotAsk
	copilotAsk = func(_ context.Context, o copilotsdk.Options, _ string) (copilotsdk.Result, error) {
		mu.Lock()
		seen = slices.Clone(o.Env)
		mu.Unlock()
		return copilotsdk.Result{Text: "hi", SessionID: "s"}, nil
	}
	t.Cleanup(func() { copilotAsk = prev })
	if _, err := Run(withIsolation(context.Background()), copilotRunner{}, Request{Prompt: "p", Provider: "alice", Direct: true}, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(seen, "HM_OTHER_KEY=") || slices.Contains(seen, "HM_OTHER_KEY=foreign-secret") {
		t.Errorf("sdk env = %v, want HM_OTHER_KEY blanked", seen)
	}
}
