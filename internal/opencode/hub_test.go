package opencode

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/settings"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// hubRegistry builds a minimal registry for hub tests: a ladder of four providers, one without an
// OpencodeID (so HubStart must reject it).
func hubRegistry() *providers.Registry {
	mk := func(id string) *providers.Provider {
		return &providers.Provider{Protocol: providers.ProtocolOpenAI, BaseURL: "https://x",
			Models: map[string]string{models.Opus: id + "-opus", models.Sonnet: id + "-sonnet"}, OpencodeID: id}
	}
	reg := &providers.Registry{
		Routing: providers.Routing{Opencode: providers.OpencodeRouting{Ladder: &[]string{"chatgpt", "github-copilot", "gemini", "nvidia"}}},
		Providers: map[string]*providers.Provider{
			"chatgpt":        mk("openai"),
			"github-copilot": mk("github-copilot"),
			"gemini":         mk("google"),
			"nvidia":         {Protocol: providers.ProtocolOpenAI, BaseURL: "https://nvidia", Models: map[string]string{models.Opus: "nv-opus"}}, // no OpencodeID
		},
	}
	for n, p := range reg.Providers {
		p.Name = n
	}
	return reg
}

// TestHubStart covers forced providers and login-aware ladder selection through the opencodeLogins seam.
func TestHubStart(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"explicit", testHubStartExplicit},
		{"fallback", testHubStartFallback},
		{"logins", testHubStartLogins},
	} {
		t.Run(tc.name, tc.run)
	}
}

func hubStartRegistry(t *testing.T) *providers.Registry {
	t.Helper()
	reg := hubRegistry()
	reg.Routing.Opencode.Ladder = &[]string{"opencode-go", "github-copilot", "openai"} // defaults.json order: Go, then Copilot, then ChatGPT
	orig := opencodeLogins
	t.Cleanup(func() { opencodeLogins = orig })
	opencodeLogins = func() ([]string, error) { return nil, nil }
	return reg
}

func testHubStartExplicit(t *testing.T) {
	t.Helper()
	reg := hubStartRegistry(t)
	// explicit provider is forced regardless of login state — "gemini" (OpencodeID "google") has no login.
	p, err := HubStart(reg, "codex", "gemini")
	if err != nil || p.Name != "gemini" {
		t.Errorf("explicit provider: p=%v err=%v, want gemini (forced, no login needed)", p, err)
	}

	// explicit provider without an OpencodeID errors.
	if _, err := HubStart(reg, "codex", "nvidia"); err == nil {
		t.Error("explicit provider without an OpencodeID must error")
	}
}

func testHubStartFallback(t *testing.T) {
	t.Helper()
	reg := hubStartRegistry(t)
	// no logins anywhere: falls to the ladder's first entry — opencode's own synthetic opencode-go provider.
	p, err := HubStart(reg, "opencode", "")
	if err != nil || p.OpencodeID != "opencode-go" {
		t.Errorf("no logins: p=%v err=%v, want the ladder's first entry opencode-go", p, err)
	}

	// an empty ladder errors — nothing to fall back to.
	empty := hubRegistry()
	empty.Routing.Opencode.Ladder = nil
	if _, err := HubStart(empty, "opencode", ""); err == nil {
		t.Error("an empty opencodeLadder must error")
	}
}

func testHubStartLogins(t *testing.T) {
	t.Helper()
	reg := hubStartRegistry(t)
	// a login further down the ladder wins over the login-less first entry.
	opencodeLogins = func() ([]string, error) { return []string{"github-copilot"}, nil }
	p, err := HubStart(reg, "opencode", "")
	if err != nil || p.Name != "github-copilot" {
		t.Errorf("github-copilot login: p=%v err=%v, want github-copilot", p, err)
	}

	// opencode's own plan carries its own login id ("opencode", not "opencode-go") and leads the ladder.
	opencodeLogins = func() ([]string, error) { return []string{"opencode"}, nil }
	p, err = HubStart(reg, "opencode", "")
	if err != nil || p.OpencodeID != "opencode-go" {
		t.Errorf("opencode-go login: p=%v err=%v, want opencode-go", p, err)
	}
}

func TestHubDispatchResolveModel(t *testing.T) {
	reg := hubRegistry()
	chatgpt, _ := reg.Get("chatgpt")
	l := hubDispatch{agent: "codex", provider: chatgpt, reg: reg}

	if got := l.resolveModel(""); got != "openai-opus" {
		t.Errorf(`resolveModel("") = %q, want provider's opus alias`, got)
	}
	if got := l.resolveModel(models.Sonnet); got != "openai-sonnet" {
		t.Errorf("resolveModel(alias name) = %q, want provider's sonnet alias", got)
	}
	if got := l.resolveModel("raw-model-id"); got != "raw-model-id" {
		t.Errorf("resolveModel(raw id) = %q, want it passed through", got)
	}
}

// hubDispatch always execs opencode with the prompt in argv (OpencodeRunArgv), so its own
// MaxPromptBytes check (hub.go, ahead of the binary lookup) stays unconditional, unlike vendorRunner's
// opencode-only gate. This fails before touching paths.AgentBin, so no bin/opencode is needed.
func TestHubDispatchRunOversizedPromptRefused(t *testing.T) {
	reg := hubRegistry()
	chatgpt, _ := reg.Get("chatgpt")
	l := hubDispatch{agent: "codex", provider: chatgpt, reg: reg}
	big := strings.Repeat("x", agent.MaxPromptBytes+10)

	_, err := l.Run(context.Background(), agent.RunOptions{Prompt: big}, wire.Discard)
	var e *retry.Error
	if !errors.As(err, &e) || !errors.Is(e, agent.ErrPromptTooLarge) {
		t.Fatalf("err = %v, want *Error wrapping ErrPromptTooLarge", err)
	}
}

// stallRun feeds lines (each after its delay) to the real stream parser wired to a IdleTimeout(window) the way Run wires
// it, and reports, once the stream has been quiet for settle, whether the idle timeout fired.
func stallRun(t *testing.T, window, settle time.Duration, lines []string, delays []time.Duration) (stalled bool) {
	t.Helper()
	w := agent.NewIdleTimeout("opencode", window, nil)
	defer w.Stop()
	firstByte, renew := w.Stop, func() { w.Beat("step_start") }
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = parseOpenCodeEvents(bufio.NewReader(pr), wire.Discard, firstByte, nil, renew)
	}()
	for i, l := range lines {
		time.Sleep(delays[i])
		if _, err := io.WriteString(pw, l+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(settle)
	stalled = errors.Is(w.Err(), agent.ErrStalled)
	pw.Close()
	<-done
	return stalled
}

// TestFirstOutputIdleTimeoutStepStart pins that step_start renews the first-output window but never disarms it:
// step_start then silence still stalls, while step_starts that keep the window alive until text arrives do not.
func TestFirstOutputIdleTimeoutStepStart(t *testing.T) {
	const step = `{"type":"step_start","sessionID":"s"}`
	const text = `{"type":"text","sessionID":"s","part":{"text":"hi"}}`
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }

	if !stallRun(t, ms(200), ms(500), []string{step}, []time.Duration{0}) {
		t.Error("step_start then silence did not stall: step_start must not disarm the idle timeout")
	}
	// text at 1000ms is past the first window (800ms); only the 500ms renewal saves it, with 300ms slack for load.
	if stallRun(t, ms(800), ms(1000), []string{step, step, text}, []time.Duration{0, ms(500), ms(500)}) {
		t.Error("step_start, step_start, text stalled: step_start must renew the window and text must disarm it")
	}
}

// TestHubRunHookBlockStartsNoProcess pins that a Block from a before-phase hook aborts the dispatch with a fatal
// agent.Error carrying the hook's reason, and that the opencode child is never launched (an allowing hook launches it).
func TestHubRunHookBlockStartsNoProcess(t *testing.T) {
	data, _ := fakeOpencode(t, "1.19.3", "", nil)
	writeAuthJSON(t, data, `{"opencode":{"type":"api","key":"k"}}`)
	marker := filepath.Join(t.TempDir(), "started")
	pathstest.FakeVendor(t, "opencode", ": > "+marker)

	orig := bindHooks
	t.Cleanup(func() { bindHooks = orig })
	decision := core.Block
	bindHooks = func() []hooks.Bound {
		return []hooks.Bound{{Group: "fake", Gate: true, Run: func(*core.Input) core.Result {
			return core.Result{Decision: decision, Reason: "prompt refused"}
		}, Spec: settings.EventSpec{Name: "memory-recall", Event: settings.EventUserPromptSubmit, Budget: time.Second}}}
	}
	p := &providers.Provider{Name: "p", OpencodeID: "opencode", Models: map[string]string{models.Opus: "m"}}
	l := hubDispatch{agent: "opencode", provider: p, reg: hubRegistry()}

	_, err := l.Run(context.Background(), agent.RunOptions{Prompt: "hi"}, wire.Discard)
	var ae *retry.Error
	if !errors.As(err, &ae) || ae.Kind != retry.KindFatal || !strings.Contains(ae.Error(), "prompt refused") {
		t.Fatalf("err = %v, want a fatal *agent.Error carrying the hook's reason", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("a blocked dispatch still started the opencode process")
	}

	decision = core.Allow
	_, err = l.Run(context.Background(), agent.RunOptions{Prompt: "hi"}, wire.Discard)
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("control: an allowing hook must launch the process, marker: %v (run err: %v)", statErr, err)
	}
}

// TestHubRunCarriesDenyAndPlugin: a hub dispatch with a Deny launches opencode with the permission pin and the gate plugin.
func TestHubRunCarriesDenyAndPlugin(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	data, _ := fakeOpencode(t, "1.19.3", "", nil)
	writeAuthJSON(t, data, `{"opencode":{"type":"api","key":"k"}}`)
	seen := filepath.Join(t.TempDir(), "env")
	pathstest.FakeVendor(t, "opencode", `{ echo "PERM=$OPENCODE_PERMISSION"; echo "CFG=$OPENCODE_CONFIG_CONTENT"; } > `+seen)
	p := &providers.Provider{Name: "p", OpencodeID: "opencode", Models: map[string]string{models.Opus: "m"}}
	l := hubDispatch{agent: "opencode", provider: p, reg: hubRegistry()}
	if l.Caps().Permissions != agent.Native {
		t.Fatal("hub Caps.Permissions must be Native once Run enforces them")
	}
	deny, err := agent.ParseRule("Bash(rm *)")
	if err != nil {
		t.Fatal(err)
	}
	req := agent.RunOptions{Prompt: "hi", Tools: true, Permissions: agent.Permissions{Deny: []agent.Rule{deny}}}
	_, _ = l.Run(context.Background(), req, wire.Discard) // the fake prints nothing, so an empty-answer error is expected
	got, _ := os.ReadFile(seen)
	for _, want := range []string{`PERM={`, `"bash"`, `harness-hooks-`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("launch env lacks %s:\n%s", want, got)
		}
	}
}

const evSecret = "AKIAIOSFODNN7EXAMPLE"

// emitScript is a fake vendor body that drains stdin then prints each NDJSON line on stdout.
func emitScript(lines ...string) string {
	var b strings.Builder
	b.WriteString("cat > /dev/null\n")
	for _, l := range lines {
		b.WriteString("printf '%s\\n' '" + l + "'\n")
	}
	return b.String()
}

// loggedRun runs fn under a WithLog (or, for a job run, WithLogEvents) ctx and returns the closed log's content.
func loggedRun(t *testing.T, jobMode bool, fn func(ctx context.Context)) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "d.log")
	sink := wire.NewLogSink(path)
	ctx := wire.WithLog(context.Background(), sink)
	if jobMode {
		ctx = wire.WithLogEvents(context.Background(), sink)
	}
	fn(ctx)
	sink.Close()
	return pathstest.ReadFile(t, path)
}

func TestLogEventOpenCodeFormatted(t *testing.T) {
	r, err := agent.Lookup("opencode")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_API_KEY", "test-key")
	var out strings.Builder
	log := loggedRun(t, false, func(ctx context.Context) {
		pathstest.FakeVendor(t, "opencode", emitScript(
			`{"type":"text","sessionID":"ses_A","part":{"text":"hello AWS_SECRET=`+evSecret+`"}}`,
			`{"type":"step_finish","sessionID":"ses_A","part":{"tokens":{"input":3,"output":2}}}`))
		if _, err := r.Run(ctx, agent.RunOptions{Prompt: "p", WorkDir: t.TempDir()}, wire.NewTextSink(&out)); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})
	if out.String() != "hello AWS_SECRET="+evSecret {
		t.Errorf("caller stream = %q", out.String())
	}
	// step_finish may or may not log a result line; the assistant line is exact and any extra line is formatted.
	lines := strings.Split(strings.TrimRight(log, "\n"), "\n")
	if lines[0] != "▸ assistant: hello AWS_SECRET=REDACTED" || len(lines) > 2 {
		t.Errorf("log lines = %q, want the assistant line first", lines)
	}
	if len(lines) == 2 && !strings.HasPrefix(lines[1], "▸ result:") {
		t.Errorf("second log line = %q, want a '▸ result:' line", lines[1])
	}
	if strings.Contains(log, evSecret) || strings.Contains(log, `{"`) {
		t.Errorf("log leaks the secret or raw JSON:\n%s", log)
	}
}

// TestHubPrepareLaunchStoredRecord: a hub dispatch fails on an invalid stored opencode record naming the field and not
// the value; a valid or absent record applies headless launch env (not args), with route env last.
func TestHubPrepareLaunchStoredRecord(t *testing.T) {
	for _, tc := range []struct {
		name, wantErr string
		rec           *vault.AgentRecord
	}{
		{"no record", "", nil},
		{"valid record", "", &vault.AgentRecord{Launch: &vault.AgentLaunch{Env: []string{"HUB_LAUNCH=1"}}}},
		{"credential-named launch.env", "launch.env", &vault.AgentRecord{Launch: &vault.AgentLaunch{Env: []string{"OPENAI_API_KEY=x"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vaulttest.Use(t)
			t.Setenv("HOME", t.TempDir())
			data, _ := fakeOpencode(t, "1.19.3", "", nil)
			writeAuthJSON(t, data, `{"opencode":{"type":"api","key":"k"}}`)
			pathstest.FakeVendor(t, "opencode", "exit 0")
			if tc.rec != nil {
				if err := vault.SetAgentRecord("opencode", *tc.rec); err != nil {
					t.Fatal(err)
				}
			}
			p := &providers.Provider{Name: "p", OpencodeID: "opencode", Models: map[string]string{models.Opus: "m"}}
			l := hubDispatch{agent: "opencode", provider: p, reg: hubRegistry()}
			req := agent.RunOptions{Prompt: "hi", Tools: true}
			_, env, argv, err := l.prepareLaunch(req, "m")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || strings.Contains(err.Error(), "OPENAI_API_KEY=x") {
					t.Fatalf("prepareLaunch err = %v, want it to name %s and not the value", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			h := agent.Headless{Prompt: "hi", Tools: true}
			wantEnv, wantArgv := opencodeRun("opencode/m", h)
			st, serr := agent.ResolveSettings("opencode", agent.RunOptions{}, providers.Target{}, true)
			if serr != nil {
				t.Fatal(serr)
			}
			launchEnv, launchedArgv, _, _, _ := agent.SchemaFor("opencode").Eng().RenderSettings(context.Background(), st, nil, wantArgv)
			wantEnv, wantArgv = agent.RouteEnvLast(launchEnv, wantEnv), launchedArgv
			if !slices.Equal(argv, wantArgv) || !slices.Equal(env[:len(wantEnv)], wantEnv) {
				t.Errorf("argv/env drifted:\n got %q %q\nwant %q %q", argv, env[:len(wantEnv)], wantArgv, wantEnv)
			}
		})
	}
}

// scratchRepo makes a throwaway git repo (no commits needed — router.IsWorkRepo only reads the origin
// remote) with origin set to url, so a test controls exactly what "the repo this dispatch runs in" looks like.
func scratchRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", origin)
	return dir
}

// setupWorkdirFixture isolates config state and makes opencode "configured" without a real login, so the
// sub-pick's gates turn on the work-repo rule alone, not on missing credentials.
func setupWorkdirFixture(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	pathstest.FakeVendor(t, "opencode", `printf '%s\n' '{"type":"text","part":{"text":"ok"}}'`)
	t.Setenv("OPENCODE_API_KEY", "test-key")
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err := vault.SetAgentRecord("opencode", vault.AgentRecord{Profile: "zen-free"}); err != nil { // no profile = off
		t.Fatal(err)
	}
}

// A work --workdir must skip free Zen even when the process cwd is a personal repo.
func TestClientPrepareWorkRepoFromWorkdirNotCwd(t *testing.T) {
	setupWorkdirFixture(t)
	if err := providers.UpdateRouting(func(r *providers.Routing) error {
		r.Opencode.WorkRepoMarkers = &[]string{"git.corp.example"}
		return nil
	}); err != nil {
		t.Fatalf("UpdateRouting() = %v", err)
	}
	personal := scratchRepo(t, "https://github.com/tester/claude-code.git")
	work := scratchRepo(t, "https://git.corp.example/scm/x/y.git")
	t.Chdir(personal)

	res := runPreparedWorkdir(t, work)
	if res.Hops[0].Account != "opencode·go" {
		t.Errorf("dispatch.Account = %q, want opencode·go (free blocked on the work repo)", res.Hops[0].Account)
	}
}

// Without --workdir, a personal process cwd still gets the free Zen dispatch.
func TestClientPrepareWorkRepoDefaultsToCwdWhenWorkdirEmpty(t *testing.T) {
	setupWorkdirFixture(t)
	personal := scratchRepo(t, "https://github.com/tester/claude-code.git")
	t.Chdir(personal)

	res := runPreparedWorkdir(t, "")
	if !strings.Contains(res.Hops[0].Account, "zen-free") {
		t.Errorf("dispatch = %+v, want free Zen on a personal cwd with no --workdir override", res.Hops[0])
	}
}

func runPreparedWorkdir(t *testing.T, workdir string) wire.Result {
	t.Helper()
	client := agent.NewClient(router.RuleClassifier{}.Alias, nil, nil)
	p, err := client.Prepare(context.Background(), agent.RunOptions{Agent: "opencode", Prompt: "test prompt",
		Fallback: router.FallbackAgents, WorkDir: workdir}, nil)
	if err != nil {
		t.Fatalf("Prepare() = %v", err)
	}
	res, err := client.Exec(context.Background(), p, nil, agent.RunRecordedOpts{})
	if err != nil || len(res.Hops) != 1 || res.Answer != "ok" {
		t.Fatalf("Exec() = %+v, %v; want one successful dispatch", res, err)
	}
	if res.DispatchID != p.Record.ID || res.DispatchID == "" {
		t.Fatalf("dispatch id = %q, want %q", res.DispatchID, p.Record.ID)
	}
	return res
}
