package copilot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

func TestResumeSlot(t *testing.T) {
	for in, want := range map[string]string{"sonnet": "sonnet", "dev:haiku": "haiku", "claude-sonnet-5-5": "sonnet", "gpt-5": ""} {
		if got := ResumeAlias(in); got != want {
			t.Errorf("ResumeAlias(%q) = %q, want %q", in, got, want)
		}
	}
}

type recSink struct{ types []string }

func (s *recSink) Emit(ev wire.Event) error { s.types = append(s.types, ev.Type); return nil }

// An approved tool then a 503 leaves the sink with a progress mark before any text; a denied-only run emits none.
func TestToolApprovalMarksProgressOnFailure(t *testing.T) {
	run := func(o SDKOptions, req copilot.PermissionRequest) []string {
		var ok atomic.Bool
		h := permissionHandler(o, &ok)
		if _, err := h(req, copilot.PermissionInvocation{}); err != nil {
			t.Fatal(err)
		}
		sink := &recSink{}
		out := SDKResult{Text: "part", ToolRan: ok.Load()}
		err := markToolRan(out, sink, errors.New("503 service unavailable")) // Run marks the tool before branching on err
		_, _ = copilotPartial(wire.Result{}, out, err, sink)
		return sink.types
	}
	if got := run(SDKOptions{AllowAllTools: true}, rpc.PermissionRequestRead{}); !slices.Equal(got, []string{wire.EventProgress, wire.EventText}) {
		t.Errorf("approved read: events = %v, want [progress text]", got)
	}
	if got := run(SDKOptions{AllowAllTools: true, ReadOnlyTools: true}, rpc.PermissionRequestMCP{}); !slices.Equal(got, []string{wire.EventText}) {
		t.Errorf("denied mcp: events = %v, want [text]", got)
	}
}

// A tool-only run (no text, no error) still marks progress, so a fallback cannot repeat its side effects.
func TestMarkToolRanMarksProgressOnSuccess(t *testing.T) {
	sink := &recSink{}
	if err := markToolRan(SDKResult{ToolRan: true}, sink, nil); err != nil {
		t.Fatal(err)
	}
	if len(sink.types) != 1 || sink.types[0] != wire.EventProgress {
		t.Fatalf("sink saw %v, want one progress mark and no tool_use", sink.types)
	}
	if err := markToolRan(SDKResult{}, sink, nil); err != nil || len(sink.types) != 1 {
		t.Fatalf("no tool ran: err=%v events=%v, want none emitted", err, sink.types)
	}
}

// A _TOKEN credential and the SDK token never land in the quota hand-off file, in prompt, partial text or session.
func TestHandoffNeverWritesCredentials(t *testing.T) {
	const sdkToken, envSecret = "gho_SDKTOKEN12345", "dbx_pat_ENVSECRET9"
	dir := t.TempDir()
	orig := handoffDir
	handoffDir = func(string) string { return dir }
	t.Cleanup(func() { handoffDir = orig })
	env := []string{providers.CopilotTokenEnv + "=" + sdkToken, "DBX_TOKEN=" + envSecret, "PLAIN=visible"}
	creds := CredentialValues(env)
	req := agent.RunOptions{WorkDir: t.TempDir(), Prompt: "use " + sdkToken + " and " + envSecret}
	path, err := writeCopilotHandoff(req, wire.Result{SessionID: "sess-1"}, "partial "+envSecret,
		"acct", errors.New("quota exhausted"), creds)
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(onDisk)
	if strings.Contains(s, sdkToken) || strings.Contains(s, envSecret) {
		t.Errorf("handoff holds a raw credential:\n%s", s)
	}
	if !strings.Contains(s, "REDACTED") {
		t.Errorf("handoff has no redaction marker:\n%s", s)
	}
}

// A failed run keeps the usage the collector summed before the failure.
func TestCopilotPartialKeepsUsage(t *testing.T) {
	want := wire.Usage{InputTokens: 7, OutputTokens: 3}
	res, _ := copilotPartial(wire.Result{}, SDKResult{Text: "partial", Usage: want}, errors.New("boom"), wire.Discard)
	if res.Usage != want {
		t.Fatalf("Usage = %+v, want %+v", res.Usage, want)
	}
}

const runGHToken = "plainRunToken123456" // no token shape, so only Run's credential list can redact it

// runScratch routes copilot to gh user "work" (fake gh/copilot, free credits), stubs the SDK; returns the hand-off dir.
func runScratch(t *testing.T, ask func(context.Context, SDKOptions, string, []string) (SDKResult, error)) string {
	t.Helper()
	pinsScratch(t)
	vaulttest.UseKeyring(t)
	gh := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", gh)
	hosts := "github.com:\n    users:\n        work:\n            oauth_token: x\n    user: work\n"
	if err := os.WriteFile(filepath.Join(gh, "hosts.yml"), []byte(hosts), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gh, "gh"), []byte("#!/bin/sh\necho "+runGHToken+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", gh)
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"github":{"personal":{"protocol":"openai",`+
		`"base_url":"https://api.githubcopilot.com","login":"work","auth":{"type":"cli","login":"copilot"},`+
		`"models":{"opus":"auto","sonnet":"auto","haiku":"auto"}}}}}`)
	pathstest.FakeVendor(t, "copilot", "exit 0")
	origPct, origAsk, origDir := CopilotUsedPct, copilotAsk, handoffDir
	dir := t.TempDir()
	CopilotUsedPct = func(context.Context, string) (float64, bool) { return 10, true }
	copilotAsk = ask
	handoffDir = func(string) string { return dir }
	t.Cleanup(func() { CopilotUsedPct, copilotAsk, handoffDir = origPct, origAsk, origDir })
	return dir
}

func runPersonal(t *testing.T) (wire.Result, error) {
	t.Helper()
	req := agent.RunOptions{Prompt: "do it", Provider: "personal", WorkDir: t.TempDir()}
	return copilotAgent.Run(context.Background(), req, wire.Discard)
}

// A quota failure on the GitHub route writes a redacted hand-off (the SDK token included) and returns a quota error.
func TestRunQuotaWritesRedactedHandoff(t *testing.T) {
	dir := runScratch(t, func(_ context.Context, o SDKOptions, _ string, _ []string) (SDKResult, error) {
		return SDKResult{Text: "half done with " + o.GitHubToken, SessionID: "abcdef1234567"}, errors.New("402 payment required")
	})
	res, err := runPersonal(t)
	if k := retry.Classify("copilot", err, false).Kind; k != retry.KindQuota {
		t.Fatalf("err = %v (kind %v), want a quota error", err, k)
	}
	if filepath.Dir(res.Handoff) != dir || res.SessionID != "abcdef1234567" || res.Provider != "personal" {
		t.Fatalf("result = %+v, want a hand-off in %s with the run's session and provider", res, dir)
	}
	body, _ := os.ReadFile(res.Handoff) // an unreadable file fails the partial-answer check below
	if s := string(body); strings.Contains(s, runGHToken) || !strings.Contains(s, "half done with REDACTED") {
		t.Errorf("hand-off leaks the SDK token or lost the partial answer:\n%s", s)
	}
}

// A non-quota failure on the same route keeps the partial answer and writes no hand-off.
func TestRunNonQuotaFailureWritesNoHandoff(t *testing.T) {
	dir := runScratch(t, func(context.Context, SDKOptions, string, []string) (SDKResult, error) {
		return SDKResult{Text: "half", SessionID: "abcdef1234567"}, errors.New("503 service unavailable")
	})
	res, err := runPersonal(t)
	if err == nil || res.Handoff != "" || res.Answer != "half" {
		t.Fatalf("res = %+v err = %v, want the partial answer, an error and no hand-off", res, err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("hand-off dir holds %d file(s), want none", len(ents))
	}
}

// Only a quota error on a copilot-billed row hands off; a BYOK row or another failure does not.
func TestQuotaHandoffEligible(t *testing.T) {
	quota, outage := errors.New("402 payment required"), errors.New("503 service unavailable")
	gh := providers.Target{Provider: &providers.Provider{Auth: providers.Auth{Login: "copilot"}}}
	byok := providers.Target{Provider: &providers.Provider{}}
	for _, c := range []struct {
		name string
		t    providers.Target
		err  error
		want bool
	}{
		{"copilot row quota", gh, quota, true},
		{"byok row quota", byok, quota, false},
		{"copilot row outage", gh, outage, false},
	} {
		if got := quotaHandoffEligible(c.t, c.err); got != c.want {
			t.Errorf("%s: eligible = %v, want %v", c.name, got, c.want)
		}
	}
}

// A target without a provider is a fatal error.
func TestInitialResult(t *testing.T) {
	cs := copilotSchema()
	if _, err := initialResult(cs, providers.Target{Model: "auto"}); !isFatal(err) {
		t.Errorf("no provider: err = %v, want a typed fatal error", err)
	}
}

func isFatal(err error) bool {
	re, ok := errors.AsType[*retry.Error](err)
	return ok && re.Kind == retry.KindFatal
}

// TestMain runs in the hermetic host (hosttest), so no test touches the live config or vault.
func TestMain(m *testing.M) {
	code := hosttest.Main(m)
	removeFakeCLI() // the fake CLI is built once per package (sdk_test.go), so it outlives every test
	os.Exit(code)
}

// The quota hand-off masks --env values and the registry token, not only the route env.
func TestRunCredentialsCoverEnvAndRegistryToken(t *testing.T) {
	opts := SDKOptions{Token: func() (string, error) { return "sess-token-123456", nil }}
	req := agent.RunOptions{Env: []string{"INTERNAL_SECRET=plain-value-12345", "EMPTY="}}
	got := runCredentials(opts, []string{"DBX_TOKEN=route-secret-1"}, req)
	for _, want := range []string{"route-secret-1", "plain-value-12345", "sess-token-123456"} {
		if !slices.Contains(got, want) {
			t.Errorf("credentials %v miss %q", got, want)
		}
	}
	if slices.Contains(got, "") {
		t.Errorf("credentials %v hold an empty value", got)
	}
}

// A plain --env value (a path) must stay out of the redaction list; secret-named ones are masked.
func TestRunCredentialsIgnoresNonSecretEnv(t *testing.T) {
	req := agent.RunOptions{Env: []string{"WORKDIR=/home/user/long/project/dir", "API_TOKEN=s3cr3t-value-123"}}
	got := runCredentials(SDKOptions{}, nil, req)
	if !slices.Equal(got, []string{"s3cr3t-value-123"}) {
		t.Fatalf("runCredentials = %q; want only the secret-named value", got)
	}
}

func TestQuotaHandoffWriteFailureKeepsPartialAnswer(t *testing.T) {
	pinsScratch(t)
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	regressionWrite(t, blocked, "blocked")
	old := handoffDir
	handoffDir = func(string) string { return filepath.Join(blocked, "handoffs") }
	t.Cleanup(func() { handoffDir = old })
	cause := errors.New("402 payment required")
	res, err := CopilotQuotaHandoff(agent.RunOptions{WorkDir: t.TempDir()}, wire.Result{}, "partial-session", "partial answer", cause, nil)
	if res.Answer != "partial answer" || res.SessionID != "partial-session" || res.Handoff != "" {
		t.Fatalf("result = %+v; want partial answer and session without handoff", res)
	}
	if !errors.Is(err, cause) || !errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), "handoff not written") {
		t.Fatalf("err = %v; want quota and handoff write errors", err)
	}
}

func regressionWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
