package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/edotau/claude-code/internal/paths"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func hmJWT(iss string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"iss":"https://`+iss+`"}`)) + ".sig"
}

// hmProfile overlays a Databricks profile "hm" on host adb-1 whose credential comes from auth.command cmd.
func hmProfile(t *testing.T, cmd string) { hmNamed(t, "hm", cmd) }

// hmNamed is hmProfile under name; command credentials are cached per name for the process, so reruns need fresh ones.
func hmNamed(t *testing.T, name, cmd string) {
	t.Helper()
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"`+name+`":{"preset":"databricks",`+
		`"host":"https://adb-1.azuredatabricks.net","auth":{"type":"key","command":`+fmt.Sprintf("%q", cmd)+`},`+
		`"models":{"opus":"databricks-claude-opus-5-5","sonnet":"databricks-claude-sonnet-5-5","haiku":"databricks-claude-haiku-4-5"}}}}}`)
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

// A routed provider:model request on a vendor agent is guarded as that provider, not skipped as an own login.
func TestGuardProviderResolvesModelPrefixForVendorAgent(t *testing.T) {
	hmProfile(t, "true")
	p, err := guardProvider(&fakeRunner{name: "codex"}, RunOptions{Model: "hm:opus"})
	if err != nil || p == nil || p.Name != "hm" {
		t.Fatalf("guardProvider = %v, %v; want the hm profile", p, err)
	}
}

// The host mismatch of a prefix-routed vendor request fails as auth and the agent never starts.
func TestRunPrefixRoutedHostMismatchFailsAuth(t *testing.T) {
	hmProfile(t, "true")
	t.Setenv("HM_KEY", hmJWT("adb-2.azuredatabricks.net"))
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"hm":{"preset":"databricks",`+
		`"host":"https://adb-1.azuredatabricks.net","auth":{"type":"key","env":"HM_KEY"},"models":{"opus":"m"}}}}}`)
	r := &fakeRunner{name: "codex", caps: Caps{Model: Native}, errs: []error{nil}}
	_, err := Run(withIsolation(context.Background()), r, RunOptions{Prompt: "p", Model: "hm:opus"}, Discard, nil)
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindAuth || len(r.seen) != 0 {
		t.Fatalf("err = %v, runs = %d; want KindAuth before any run", err, len(r.seen))
	}
}

// A transient credential-command failure is retried inside Run instead of failing the hop as auth.
func TestRunTransientCredentialFailureIsRetried(t *testing.T) {
	fastDefaultBackoff(t)
	flag := filepath.Join(t.TempDir(), "seen")
	name := fmt.Sprintf("hmt%d", time.Now().UnixNano())
	hmNamed(t, name, fmt.Sprintf("if [ -f %[1]s ]; then echo %[2]s; else touch %[1]s; echo 'connection reset' >&2; exit 1; fi", flag, hmJWT("adb-1.azuredatabricks.net")))
	r := &fakeRunner{name: "claude", caps: Caps{Provider: Native}, errs: []error{nil}}
	res, err := Run(context.Background(), r, RunOptions{Prompt: "p", Provider: name}, Discard, nil)
	if err != nil || res.Answer != "a" || res.Transport.Attempts != 2 {
		t.Fatalf("res=%+v err=%v; want an answer on attempt 2", res, err)
	}
}

// An auth.command whose JWT was issued for another host fails the hop as auth, and fallback moves to the next hop.
func TestCommandCredentialHostMismatchIsAuthAndFallsBack(t *testing.T) {
	name := fmt.Sprintf("hmm%d", time.Now().UnixNano())
	vaulttest.Put(t, vault.Overlay, "providers", `{"default":"`+name+`","providers":{"databricks":{"`+name+`":{"preset":"databricks",`+
		`"host":"https://adb-1.azuredatabricks.net","auth":{"type":"key","command":"echo `+hmJWT("adb-2.azuredatabricks.net")+`"},`+
		`"models":{"opus":"m"}}}}}`)
	t.Cleanup(func() { vaulttest.Put(t, vault.Overlay, "providers", `{}`) }) // the default must not leak to later tests
	prev := markAuth
	markAuth = func(string, string) error { return nil }
	t.Cleanup(func() { markAuth = prev })
	first := &fakeRunner{name: "claude", caps: Caps{Provider: Native}, errs: []error{nil}}
	second := &fakeRunner{name: "gemini", errs: []error{nil}}
	c := Chain{Want: "claude", Dispatches: []Dispatch{{Agent: first, Account: "claude:" + name, keepCaller: true}, {Agent: second}}}
	res, err := RunWithFallback(context.Background(), c, RunOptions{Prompt: "p"}, Discard, nil)
	if err != nil || res.Answer != "a" || len(first.seen) != 0 || len(second.seen) != 1 {
		t.Fatalf("res=%+v err=%v first=%d second=%d; want hop 2 to answer", res, err, len(first.seen), len(second.seen))
	}
	if len(res.Hops) != 2 || res.Hops[0].ErrorKind != "auth" {
		t.Errorf("hops = %+v, want hop 1 error_kind auth", res.Hops)
	}
}

// Under batch isolation an own-login child never sees another profile's custom credential var, but keeps the
// native var its launch sets; outside a batch nothing is scrubbed.
func TestChildCmdOwnLoginDropsForeignCredentialsUnderIsolation(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{`+
		`"pa":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"PA_BATCH_KEY"},"models":{"opus":"m"}},`+
		`"pb":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"OPENCODE_API_KEY"},"models":{"opus":"m"}}}}}`)
	t.Setenv("PA_BATCH_KEY", "foreign-secret")
	has := func(cmd interface{ Environ() []string }, kv string) bool { return slices.Contains(cmd.Environ(), kv) }
	iso, _ := ChildCmd(withIsolation(context.Background()), "/bin/true", nil, []string{"OPENCODE_API_KEY=native"}, "", "", nil, true)
	if has(iso, "PA_BATCH_KEY=foreign-secret") || !has(iso, "OPENCODE_API_KEY=native") {
		t.Errorf("isolated env leaks PA_BATCH_KEY or lost the native var: %v", iso.Env)
	}
	plain, _ := ChildCmd(context.Background(), "/bin/true", nil, nil, "", "", nil, true)
	if !has(plain, "PA_BATCH_KEY=foreign-secret") {
		t.Error("outside a batch the env must stay unchanged")
	}
}

// Two batches (separate processes in effect) stamping the same millisecond never share a record id or file.
func TestReserveIsExclusiveAcrossBatches(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			ids[i] = ClaimRecord("codex", RunOptions{Prompt: "p"}, now).ID // the one allocator every dispatch path uses
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("record id %s handed out twice: %v", id, ids)
		}
		seen[id] = true
		if _, err := os.Stat(filepath.Join(paths.DispatchDir(), id+".json")); err != nil {
			t.Errorf("record %s has no claimed file: %v", id, err)
		}
	}
}

// A hub dispatch keeps the credential its launch supplies even when another profile declares the same env name; the
// inherited value of that name is gone.
func TestChildCmdLaunchValuesSurviveIsolationScrub(t *testing.T) {
	vaulttest.UseKeyring(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"anthropic":{`+
		`"ocgo":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"OCGO_KEY"},"models":{"opus":"m"}},`+
		`"other":{"protocol":"anthropic","base_url":"http://127.0.0.1:1","auth":{"type":"key","env":"OPENCODE_API_KEY"},"models":{"opus":"m"}}}}}`)
	t.Setenv("OPENCODE_API_KEY", "inherited-foreign")
	reg, err := providers.Load()
	if err != nil || reg.Providers["ocgo"] == nil {
		t.Fatalf("registry: %v", err)
	}
	ctx := withOwnProvider(withIsolation(context.Background()), reg.Providers["ocgo"])
	cmd, _ := ChildCmd(ctx, "/bin/true", nil, []string{"OPENCODE_API_KEY=launch"}, "", "", nil, true)
	if !slices.Contains(cmd.Env, "OPENCODE_API_KEY=launch") || slices.Contains(cmd.Env, "OPENCODE_API_KEY=inherited-foreign") {
		t.Errorf("env keeps the inherited foreign value or lost the launch's: %v", slices.DeleteFunc(slices.Clone(cmd.Env),
			func(kv string) bool { return !strings.HasPrefix(kv, "OPENCODE_API_KEY") }))
	}
}

// A reserved record is a complete running record from its first byte, so a concurrent prune keeps it.
func TestReservedRecordSurvivesPrune(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := dispatchKeep
	dispatchKeep = 1
	t.Cleanup(func() { dispatchKeep = old })
	dir := paths.DispatchDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	done := Record{Schema: DispatchSchema, ID: "99991231T000000.000-zz", Agent: "zz", Status: "ok"}
	if err := done.Save(); err != nil {
		t.Fatal(err)
	}
	rec := ClaimRecord("aa", RunOptions{Prompt: "p"}, time.Now())
	pruneDispatches(dir) // lands between reserve and Save
	if _, err := os.Stat(filepath.Join(dir, rec.ID+".json")); err != nil {
		t.Errorf("the reserved record was pruned before Save: %v", err)
	}
}

// A 429 followed by success is one hop, no error_kind, whose errors list the recovered rate_limit attempt.
func TestHopKeepsRecoveredAttemptErrors(t *testing.T) {
	fastDefaultBackoff(t)
	r := &fakeRunner{name: "claude", errs: []error{&Error{Kind: KindRateLimit, Agent: "claude", Err: errors.New("HTTP 429")}, nil}}
	res, err := RunWithFallback(context.Background(), Single(r), RunOptions{Prompt: "p"}, Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	env := envelopeMap(t, res, err)
	hops, _ := env["hops"].([]any)
	if len(hops) != 1 {
		t.Fatalf("hops = %v, want one", env["hops"])
	}
	h, _ := hops[0].(map[string]any)
	errs, _ := h["errors"].([]any)
	if h["error_kind"] != nil || len(errs) != 1 || errs[0].(map[string]any)["kind"] != "rate_limit" {
		t.Errorf("hop = %v, want no error_kind and one rate_limit error", h)
	}
}

// The final attempt's 429 still publishes its cooldown: a second Do on the same key waits for it.
func TestDoPublishesCooldownOnFinalAttempt(t *testing.T) {
	key := "final-" + t.Name()
	p := Policy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Second, Key: key}
	Do(context.Background(), p, func(context.Context, int) *Error {
		return &Error{Kind: KindRateLimit, Agent: "a", RetryAfter: 300 * time.Millisecond, Err: errors.New("429")}
	})
	start := time.Now()
	Do(context.Background(), p, func(context.Context, int) *Error { return nil })
	if got := time.Since(start); got < 200*time.Millisecond {
		t.Errorf("second Do started after %v, want it held for the published ~300ms cooldown", got)
	}
}

// A transient credential failure that a retry recovers from is listed in the answering hop's errors.
func TestHopKeepsRecoveredGuardFailure(t *testing.T) {
	fastDefaultBackoff(t)
	flag := filepath.Join(t.TempDir(), "seen")
	name := fmt.Sprintf("hmg%d", time.Now().UnixNano())
	hmNamed(t, name, fmt.Sprintf("if [ -f %[1]s ]; then echo %[2]s; else touch %[1]s; echo 'connection reset' >&2; exit 1; fi", flag, hmJWT("adb-1.azuredatabricks.net")))
	r := &fakeRunner{name: "claude", caps: Caps{Provider: Native}, errs: []error{nil}}
	res, err := RunWithFallback(context.Background(), Single(r), RunOptions{Prompt: "p", Provider: name}, Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	hops, _ := envelopeMap(t, res, err)["hops"].([]any)
	h, _ := hops[0].(map[string]any)
	if errs, _ := h["errors"].([]any); len(errs) != 1 {
		t.Errorf("hop = %v, want the recovered credential failure in errors", h)
	}
}

