package gemini

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/agenttest"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// agyArgv maps Autonomous onto "--mode auto"; --dangerously-skip-permissions rides on Tools alone, so
// an autonomous-but-tool-less run must never carry it (agy must never auto-grant write access it wasn't
// asked for).
func TestAgyArgvAutonomous(t *testing.T) {
	cases := []struct {
		name               string
		tools, autonomous  bool
		wantMode, wantSkip bool
	}{
		{"neither", false, false, false, false},
		{"tools only", true, false, false, true},
		{"autonomous only", false, true, true, false},
		{"both", true, true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			argv := agyArgv(agyRequest{Model: "m", Tools: c.tools, Autonomous: c.autonomous})
			hasMode := strings.Contains(strings.Join(argv, " "), "--mode auto")
			hasSkip := slices.Contains(argv, "--dangerously-skip-permissions")
			if hasMode != c.wantMode || hasSkip != c.wantSkip {
				t.Errorf("argv = %q, mode=%v skip=%v, want mode=%v skip=%v", argv, hasMode, hasSkip, c.wantMode, c.wantSkip)
			}
		})
	}
}

// AgyRequest.SessionID rides as "--conversation <id>"; an empty SessionID must never emit the flag
// (agy has no bare --conversation value to resume against).
func TestAgyArgvConversation(t *testing.T) {
	cases := []struct{ name, session string }{{"unset", ""}, {"set", "conv-123"}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			argv := agyArgv(agyRequest{Model: "m", SessionID: c.session})
			idx := slices.Index(argv, "--conversation")
			if has := idx >= 0; has != (c.session != "") {
				t.Fatalf("argv = %q, --conversation present = %v, want %v", argv, has, c.session != "")
			}
			if c.session != "" && argv[idx+1] != c.session {
				t.Errorf("argv = %q, --conversation value = %q, want %q", argv, argv[idx+1], c.session)
			}
		})
	}
}

// agyFixture is a hermetic vault and HOME with agy's Google login marker; rec, when set, is the stored gemini record.
func agyFixture(t *testing.T, rec *vault.AgentRecord) {
	t.Helper()
	vaulttest.Use(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_PROFILE", "")
	t.Setenv("HARNESS_PROVIDER", "")
	t.Cleanup(agent.StubRouter("http://127.0.0.1:9999"))
	marker := AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec != nil {
		if err := vault.SetAgentRecord("gemini", *rec); err != nil {
			t.Fatal(err)
		}
	}
}

// Effort and --settings ride just before -p=; a flag beats the record; a suffixed model's effort is emulated.
func TestAgyArgvSettings(t *testing.T) {
	stream := []string{"--input-format", "stream-json", "--output-format", "stream-json"}
	settings := `{"permissions":{"deny":["command(rm -rf /)","command(rm -rf ~)","write_file(/w/x.txt)","edit_file(/w/x.txt)"]}}`
	deny := agent.Permissions{Deny: []agent.Rule{{Tool: "Write", Arg: "x.txt"}}}
	high := &vault.AgentRecord{Effort: "high"}
	for _, c := range []struct {
		name         string
		rec          *vault.AgentRecord
		req          agent.RunOptions
		want         []string
		wantEmulated []string
	}{
		{"no record", nil, agent.RunOptions{}, slices.Concat([]string{"--model", agyDefaultModel}, stream, []string{"-p="}), nil},
		{"record effort", high, agent.RunOptions{Model: "gemini-3.8-flash"},
			slices.Concat([]string{"--model", "gemini-3.8-flash"}, stream, []string{"--effort", "high", "-p="}), nil},
		{"flag over record", high, agent.RunOptions{Model: "gemini-3.8-flash", Effort: "low"},
			slices.Concat([]string{"--model", "gemini-3.8-flash"}, stream, []string{"--effort", "low", "-p="}), nil},
		{"suffixed model", high, agent.RunOptions{}, slices.Concat([]string{"--model", agyDefaultModel}, stream, []string{"-p="}),
			[]string{"effort high: model " + agyDefaultModel + " carries its own"}},
		{"settings before -p=", high, agent.RunOptions{Model: "gemini-3.8-flash", Tools: true, WorkDir: "/w", Permissions: deny},
			slices.Concat([]string{"--model", "gemini-3.8-flash"}, stream,
				[]string{"--dangerously-skip-permissions", "--effort", "high", "--settings", settings, "-p="}), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			agyFixture(t, c.rec)
			c.req.Prompt = "hi"
			p, err := agent.PrepareCLI(context.Background(), "gemini", c.req)
			if err != nil {
				t.Fatal(err)
			}
			if got := agyArgv(newAgyRequest(p, c.req)); !slices.Equal(got, c.want) {
				t.Errorf("argv = %q\nwant   %q", got, c.want)
			}
			if !slices.Equal(p.Emulated, c.wantEmulated) {
				t.Errorf("emulated = %q, want %q", p.Emulated, c.wantEmulated)
			}
		})
	}
}

// A stored effort outside gemini's list refuses the dispatch instead of being dropped.
func TestAgyUnknownRecordEffortIsRefused(t *testing.T) {
	agyFixture(t, &vault.AgentRecord{Effort: "bogus"})
	if _, err := agent.PrepareCLI(context.Background(), "gemini", agent.RunOptions{Prompt: "hi"}); err == nil || !strings.Contains(err.Error(), `"bogus"`) {
		t.Fatalf("err = %v, want the unknown effort refused", err)
	}
}

func TestAgyArgvDebugFile(t *testing.T) {
	if slices.Contains(agyArgv(agyRequest{Model: "m"}), "--log-file") {
		t.Fatal("--log-file without DebugFile")
	}
	argv := agyArgv(agyRequest{Model: "m", DebugFile: "/tmp/l"})
	if i := slices.Index(argv, "--log-file"); i < 0 || argv[i+1] != "/tmp/l" {
		t.Fatalf("argv = %v", argv)
	}
}

// Deltas already parsed survive a later typed-decode failure.
func TestParseAgyStreamDecodeErrorKeepsText(t *testing.T) {
	stream := `{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"hi"}}
{"event":123}`
	res, err := parseAgyStream(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
	if err == nil || res.Answer != "hi" {
		t.Fatalf("res = %+v, err = %v; want the decode error with the text kept", res, err)
	}
}

type agySinkRec struct{ types []string }

func (s *agySinkRec) Emit(ev wire.Event) error { s.types = append(s.types, ev.Type); return nil }

// A tool step marks progress (one progress mark per step, never a tool_use) before a later decode failure; planning steps do not.
func TestParseAgyToolStepMarksProgress(t *testing.T) {
	run := func(stepType string) []string {
		stream := `{"event":"step_update","step_update":{"step_type":"` + stepType + `","state":"ACTIVE"}}
{"event":"step_update","step_update":{"step_type":"` + stepType + `","state":"ACTIVE"}}
{"event":"step_update","step_update":{"step_type":"` + stepType + `","state":"DONE"}}
{"event":123}`
		sink := &agySinkRec{}
		if _, err := parseAgyStream(bufio.NewReader(strings.NewReader(stream)), sink, nil); err == nil {
			t.Fatal("want the decode error")
		}
		if slices.Contains(sink.types, wire.EventToolUse) {
			t.Errorf("%s step: placeholder tool_use reached the sink: %v", stepType, sink.types)
		}
		return slices.DeleteFunc(sink.types, func(ty string) bool { return ty != wire.EventProgress })
	}
	if got := run("tool"); len(got) != 1 {
		t.Errorf("tool step: %d progress marks, want 1", len(got))
	}
	if got := run("planner"); len(got) != 0 {
		t.Errorf("planning step: %d progress marks, want 0", len(got))
	}
}

// A result carrying only structured_output (no response text) is a success and keeps the structure.
func TestParseAgyStructuredOnlySucceeds(t *testing.T) {
	stream := `{"event":"result","result":{"status":"SUCCESS","structured_output":{"k":1}}}`
	res, err := parseAgyStream(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
	if err != nil || string(res.Structured) != `{"k":1}` {
		t.Fatalf("res = %+v, err = %v; want no error and Structured kept", res, err)
	}
}

// A result.response-only answer survives a decode error that follows the result event.
func TestParseAgyResponseSurvivesLaterDecodeError(t *testing.T) {
	stream := `{"event":"result","result":{"status":"SUCCESS","response":"R"}}
{"event":123}`
	res, err := parseAgyStream(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
	if err == nil || res.Answer != "R" {
		t.Fatalf("res = %+v, err = %v; want the decode error with Answer R", res, err)
	}
}

// A tool step a pre-tool hook refused ends in ERROR; the answer that follows is a success, not a partial result.
func TestParseAgyRefusedToolStepIsNotPartial(t *testing.T) {
	stream := `{"event":"step_update","step_update":{"step_type":"tool","state":"ACTIVE"}}
{"event":"step_update","step_update":{"step_type":"tool","state":"ERROR"}}
{"event":"result","result":{"status":"SUCCESS","response":"blocked as asked"}}`
	res, err := parseAgyStream(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
	if err != nil || res.Answer != "blocked as asked" {
		t.Fatalf("res.Answer = %q, err = %v; want the answer and no partial error", res.Answer, err)
	}
}

// agy refuses --effort beside an effort-suffixed id ("conflicts with --effort"); a bare id keeps the flag.
func TestAgyArgvEffortSuffixedModelDropsEffort(t *testing.T) {
	for model, want := range map[string]bool{"gemini-3.8-flash-high": false, "gemini-3.8-flash": true} {
		argv := agyArgv(agyRequest{Model: model, Effort: "medium"})
		if got := slices.Contains(argv, "--effort"); got != want {
			t.Errorf("%s: --effort present = %v, want %v (%v)", model, got, want, argv)
		}
	}
}

// A non-Gemini model (and whatever the pin is) must never leave the agy gateway model empty.
func TestAgyGatewayArgNonGeminiNeverEmpty(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	base, arg, err := agyGatewayArg("claude-opus-4-1")
	if err != nil || base == "" || arg == "" {
		t.Fatalf("agyGatewayArg(non-Gemini) = (%q, %q, %v), want a non-empty Gemini base", base, arg, err)
	}
}

// An unreadable login marker is an error, not "not signed in", and the probe reports it.
func TestGeminiLoginUnreadableMarkerIsError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "antigravity-oauth-token"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if ok, err := geminiLoginProbe(); ok || err == nil {
		t.Fatalf("geminiLoginProbe() = (%v, %v), want (false, error)", ok, err)
	}
	_, _, err := geminiLogin("gemini-3.8-flash", nil)
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("geminiLogin err = %v, want an unreadable-marker error", err)
	}
}

// A turn that errors before any result event must not reset the usage baseline: the next delta stays a delta.
func TestAgyLiveErroredTurnKeepsUsageBaseline(t *testing.T) {
	result := func(in int) string {
		return `{"event":"result","result":{"status":"SUCCESS","response":"r","usage":{"input_tokens":` + strconv.Itoa(in) + `}}}`
	}
	b := &agyLive{}
	parse := func(stream string) (wire.Result, error) {
		return b.Parse(bufio.NewReader(strings.NewReader(stream)), wire.Discard, nil)
	}
	if _, err := parse(result(1000)); err != nil {
		t.Fatal(err)
	}
	if _, err := parse(`{"event":123}`); err == nil {
		t.Fatal("want the decode error on the errored turn")
	}
	res, err := parse(result(1500))
	if err != nil || res.Usage.InputTokens != 500 {
		t.Fatalf("turn 3 InputTokens = %d, err = %v; want the 500 delta", res.Usage.InputTokens, err)
	}
}

func TestGeminiGatewayInteractiveModelMatchesAdvertised(t *testing.T) {
	t.Cleanup(agent.StubRouter("http://127.0.0.1:19876"))
	origMint := agent.MintSession
	agent.MintSession = func(context.Context, []string, int) (string, error) { return "test-session", nil }
	t.Cleanup(func() { agent.MintSession = origMint })
	p := &providers.Provider{Name: t.Name(), Preset: providers.PresetDatabricks, Auth: providers.Auth{Type: "pat"}}
	env, argv, err := geminiGateway(context.Background(), p, "gemini-3.8-flash-high", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "AGY_LLM_GATEWAY_MODELS=databricks-gemini-3-8-flash") {
		t.Fatalf("gateway must advertise the base model: %v", env)
	}
	want := []string{"--model", "databricks-gemini-3-8-flash", "--effort", "high"}
	if !slices.Equal(argv, want) {
		t.Fatalf("interactive gateway argv = %v, want %v to match the advertised model", argv, want)
	}
}

// On a Databricks gateway row agy lists only the base id, so the effort suffix must ride --effort, not --model.
func TestAgyGatewayModelIsBaseWithEffort(t *testing.T) {
	gw := providers.Target{Provider: &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks, Auth: providers.Auth{Type: "pat"}},
		Model: "gemini-3.8-flash-high"}
	req := agent.RunOptions{Model: "gemini-3.8-flash-high"}
	m, e := agyModelEffort(req, gw)
	if strings.HasSuffix(m, "-high") || m == "" {
		t.Fatalf("gateway --model = %q, want the base id", m)
	}
	if e != "high" {
		t.Fatalf("gateway --effort = %q, want high", e)
	}
}

// pinFixture is a hermetic registry with two Databricks gateway rows (dev, gw), a fake agy Google login and the session on session.
func pinFixture(t *testing.T, session string) {
	t.Helper()
	vaulttest.Use(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_PROFILE", session)
	t.Cleanup(agent.StubRouter("http://127.0.0.1:9999"))
	row := func(host string) string {
		return `{"preset":"databricks","host":"https://` + host + `.example.com","auth":{"type":"pat"},"models":{"opus":"databricks-claude-opus"}}`
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"databricks":{"dev":`+row("dev")+`,"gw":`+row("gw")+`}}}`)
	marker := filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func resolveGemini(t *testing.T, req agent.RunOptions) (providers.Target, bool, error) {
	t.Helper()
	agent.NoticeOut = &strings.Builder{}
	return agent.SchemaFor("gemini").TargetCtx(context.Background(), req)
}

// The gemini vault record beats the Databricks session profile: profile picks the row, model the id, flags beat both.
func TestGeminiRecordBeatsSessionProfile(t *testing.T) {
	for _, c := range []struct {
		name, session, profile, model string
		req                           agent.RunOptions
		wantProv, wantModel, wantErr  string
	}{
		{"A model pin on the Google login", "dev", "", "gemini-pinned", agent.RunOptions{}, "antigravity", "gemini-pinned", ""},
		{"B pinned gateway row beats the session row", "dev", "gw", "gemini-y", agent.RunOptions{}, "gw", "gemini-y", ""},
		{"C pin names another row than the gateway session", "gw", "dev", "", agent.RunOptions{}, "dev", "", ""},
		{"C gateway session without a pin stays on the Google login", "gw", "", "", agent.RunOptions{}, "antigravity", "", ""},
		{"D --model beats the record model", "dev", "gw", "gemini-y", agent.RunOptions{Model: "gemini-flag"}, "gw", "gemini-flag", ""},
		{"D --provider beats the record profile", "dev", "gw", "", agent.RunOptions{Provider: "dev"}, "dev", "", ""},
		{"E own-login label is a valid pin", "dev", "antigravity", "", agent.RunOptions{}, "antigravity", "", ""},
		{"E pin naming no registry row is refused", "dev", "nosuch", "", agent.RunOptions{}, "", "", `pinned profile: gemini: unknown profile "nosuch"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			pinFixture(t, c.session)
			vaulttest.SetRecord(t, "gemini", "profile", c.profile)
			vaulttest.SetRecord(t, "gemini", "model", c.model)
			tg, _, err := resolveGemini(t, c.req)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, c.wantErr)
				}
				return
			}
			if err != nil || tg.Provider == nil || tg.Provider.Name != c.wantProv {
				t.Fatalf("target = %+v err = %v, want provider %s", tg, err, c.wantProv)
			}
			if c.wantModel != "" && tg.Model != c.wantModel {
				t.Errorf("model = %q, want %q", tg.Model, c.wantModel)
			}
		})
	}
}

// A gateway session row that no record names is refused when agy has no Google login to fall back on.
func TestGeminiGatewaySessionWithoutPinOrLoginIsRefused(t *testing.T) {
	pinFixture(t, "gw")
	if err := os.RemoveAll(filepath.Join(os.Getenv("HOME"), ".gemini")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveGemini(t, agent.RunOptions{}); err == nil || !strings.Contains(err.Error(), "Google login") {
		t.Fatalf("err = %v, want the Google-login refusal", err)
	}
}

func TestVendorPassesThroughWhenUnroutable(t *testing.T) {
	agenttest.LaunchHermetic(t)
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CLAUDE_PROFILE", "ultra") // passthrough auth: gemini cannot use it
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("HOME", t.TempDir()) // no vendor login state on disk
	fake := pathstest.FakeVendor(t, "agy", "")
	offers := 0
	agent.OfferConfigure = func(vendor string, reason error) bool { offers++; return false }
	t.Cleanup(func() { agent.OfferConfigure = nil })

	plan, err := agent.LaunchPlan(context.Background(), "gemini", agent.LaunchOptions{}, []string{"-p", "hi"})
	if err != nil {
		t.Fatalf("expected a passthrough plan, got %v", err)
	}
	// gemini's own built-in vendor spec (agentconfig defaults) still prepends --dangerously-skip-permissions.
	wantArgv := []string{"--dangerously-skip-permissions", "-p", "hi"}
	if plan.Binary != fake || !slices.Equal(plan.Argv, wantArgv) || len(plan.Set) != 0 {
		t.Errorf("plan = %+v, want the plain CLI with %v prepended and no harness env", plan, wantArgv)
	}
	if offers != 1 {
		t.Errorf("configure offered %d times, want 1", offers)
	}
	// --print-env never prompts.
	offers = 0
	if _, err := agent.LaunchPlan(context.Background(), "gemini", agent.LaunchOptions{PrintEnv: true}, nil); err != nil || offers != 0 {
		t.Errorf("print-env: err=%v offers=%d", err, offers)
	}
}

func TestConfigureDeclinedPreservesModelFlag(t *testing.T) {
	agenttest.LaunchHermetic(t)
	fake := pathstest.FakeVendor(t, "agy", "")
	const model = "gemini-3.8-flash-high"
	o, args, err := agent.ParseArgs([]string{"--profile", "antigravity", "--model", model, "-p", "hi"}, false)
	if err != nil {
		t.Fatal(err)
	}
	offers := 0
	original := agent.OfferConfigure
	agent.OfferConfigure = func(name string, reason error) bool {
		offers++
		if name != "gemini" || !strings.Contains(reason.Error(), "not signed in") {
			t.Errorf("configure offer = %q, %v; want missing Gemini login", name, reason)
		}
		return false
	}
	t.Cleanup(func() { agent.OfferConfigure = original })
	plan, err := agent.LaunchPlan(t.Context(), "gemini", o, args)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--dangerously-skip-permissions", "--model", model, "-p", "hi"}
	if offers != 1 || plan.Binary != fake || len(plan.Set) != 0 || !slices.Equal(plan.Argv, want) {
		t.Fatalf("offers=%d plan=%+v; want plain CLI with argv %v", offers, plan, want)
	}
}

func TestConfigureRetryPreservesAgentModelPin(t *testing.T) {
	agenttest.LaunchHermetic(t)
	fake := pathstest.FakeVendor(t, "agy", "")
	const model = "gemini-3.8-flash-low"
	vaulttest.SetRecord(t, "gemini", "model", model)
	offers := 0
	original := agent.OfferConfigure
	agent.OfferConfigure = func(name string, reason error) bool {
		offers++
		if name != "gemini" || !strings.Contains(reason.Error(), "not signed in") {
			t.Errorf("configure offer = %q, %v; want missing Gemini login", name, reason)
		}
		marker := filepath.Join(os.Getenv("HOME"), ".gemini", "antigravity-cli", "antigravity-oauth-token")
		if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte("test-login"), 0o600); err != nil {
			t.Fatal(err)
		}
		return true
	}
	t.Cleanup(func() { agent.OfferConfigure = original })
	plan, err := agent.LaunchPlan(t.Context(), "gemini", agent.LaunchOptions{Provider: "antigravity"}, []string{"-p", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--model", model, "--dangerously-skip-permissions", "-p", "hi"}
	if offers != 1 || plan.Binary != fake || !slices.Equal(plan.Argv, want) {
		t.Fatalf("offers=%d plan=%+v; want configured CLI with pinned model in argv %v", offers, plan, want)
	}
}

// liveEnv gives the test a short /tmp config dir (a unix socket path caps at 104 bytes on macOS, 108 on Linux).
func liveEnv(t *testing.T) {
	t.Helper()
	// the agy target needs a Google sign-in marker; never read the developer's
	marker := AgyLoginMarker(pathstest.ShortConfigDir(t))
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeLiveAgy is a shell agy: one init, then per NDJSON turn an agent_response delta and a result whose usage is
// cumulative over the conversation, as the real agy reports it.
const fakeLiveAgy = `#!/bin/sh
printf '{"event":"init","conversation_id":"c1"}\n'
mem=none
n=0
while IFS= read -r line; do
  n=$((n + 1))
  m=$(printf '%s' "$line" | sed -n 's/.*remember \([0-9][0-9]*\).*/\1/p')
  if [ -n "$m" ]; then mem=$m; ans=ok; else ans="$mem"; fi
  printf '{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"%s"}}\n' "$ans"
  printf '{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"%s","usage":{"input_tokens":%d,"output_tokens":%d}}}\n' "$ans" $((n * 10)) $((n * 2))
done
`

// An agy session keeps its conversation across turns, reports per-turn usage, refuses a concurrent send, and cancels.
func TestLiveAgySession(t *testing.T) {
	liveEnv(t)
	if err := os.WriteFile(filepath.Join(paths.BinDir(), "gemini"), []byte(fakeLiveAgy), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	id, wait := agenttest.StartHolder(t, ctx, "gemini", time.Minute, nil)
	if _, err := os.Stat(agent.LiveSocket(id)); err != nil {
		cancel()
		t.Fatalf("holder did not open its socket: %v (%v)", err, wait())
	}
	assertAgyRecallAndUsage(t, id)
	cancel()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	got, _ := agent.LoadRecord(id)
	if got.Status != "error" || got.Error != "cancelled" || got.Agent != "gemini" || got.SessionID != "c1" {
		t.Errorf("record = %s %q agent=%s session=%q, want error cancelled gemini c1", got.Status, got.Error, got.Agent, got.SessionID)
	}
}

func assertAgyRecallAndUsage(t *testing.T, id string) {
	t.Helper()
	env, err := agent.SendLive(context.Background(), id, "remember 42", wire.Discard)
	if err != nil || env.Status != wire.StatusOK || env.Answer != "ok" || env.Agent != "gemini" {
		t.Fatalf("turn 1 = %+v, %v", env, err)
	}
	env2, err := agent.SendLive(context.Background(), id, "what number", wire.Discard)
	if err != nil || env2.Answer != "42" {
		t.Fatalf("turn 2 = %+v, %v (the second turn must recall the first)", env2, err)
	}
	if env2.Usage.InputTokens != 10 || env2.Usage.OutputTokens != 2 {
		t.Errorf("turn 2 usage = %+v, want the turn's own delta 10/2, not agy's cumulative 20/4", env2.Usage)
	}
}

// fakeErrAgy fails the turn "fail" with an ERROR result and answers everything else.
const fakeErrAgy = `#!/bin/sh
printf '{"event":"init","conversation_id":"c1"}\n'
while IFS= read -r line; do
  case "$line" in
    *fail*) printf '{"event":"result","result":{"conversation_id":"c1","status":"ERROR","error":"upstream failed"}}\n' ;;
    *) printf '{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"%s","usage":{"input_tokens":50,"output_tokens":5}}}\n' "$(printf '%s' "$line" | tr -d '\\"' | head -c 3000 | sed 's/.*content://')" ;;
  esac
done
`

func startAgy(t *testing.T, script string, req agent.RunOptions) (string, func(), func() error) {
	t.Helper()
	liveEnv(t)
	if err := os.WriteFile(filepath.Join(paths.BinDir(), "gemini"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := agent.ClaimRecord("gemini", req, time.Now())
	rec.Live, rec.PID = true, os.Getpid()
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.ServeLive(ctx, &rec) }()
	for range 200 {
		if agenttest.SocketAccepts(agent.LiveSocket(rec.ID)) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	return rec.ID, cancel, func() error { return <-done }
}

// An agy ERROR result fails that turn only: the session stays usable for the next one.
func TestLiveAgyErrorResultKeepsSession(t *testing.T) {
	id, cancel, wait := startAgy(t, fakeErrAgy, agent.RunOptions{Timeout: time.Minute})
	defer func() { cancel(); _ = wait() }()
	env, err := agent.SendLive(context.Background(), id, "fail now", wire.Discard)
	if err == nil && env.Status != wire.StatusError {
		t.Fatalf("failing turn = %+v, %v; want an error status", env, err)
	}
	if env2, err := agent.SendLive(context.Background(), id, "again", wire.Discard); err != nil || env2.Status != wire.StatusOK {
		t.Fatalf("turn after an ERROR result = %+v, %v; the session must survive", env2, err)
	}
}

// System prompts are emulated on gemini live as the one-shot path does: prepended to the first turn only.
func TestLiveAgySystemPromptOnFirstTurn(t *testing.T) {
	id, cancel, wait := startAgy(t, fakeErrAgy, agent.RunOptions{Timeout: time.Minute, AppendSystemPrompt: "SPEAK-FRENCH"})
	defer func() { cancel(); _ = wait() }()
	e1, err := agent.SendLive(context.Background(), id, "hello", wire.Discard)
	if err != nil || !strings.Contains(e1.Answer, "SPEAK-FRENCH") {
		t.Fatalf("turn 1 = %+v, %v; want the system prompt in the first turn", e1, err)
	}
	e2, err := agent.SendLive(context.Background(), id, "hello", wire.Discard)
	if err != nil || strings.Contains(e2.Answer, "SPEAK-FRENCH") {
		t.Fatalf("turn 2 = %+v, %v; the system prompt must not repeat", e2, err)
	}
}

// A resumed agy conversation reports no usage for its first turn (the history is unknown), then per-turn deltas.
func TestLiveAgyResumedUsageBaseline(t *testing.T) {
	id, cancel, wait := startAgy(t, fakeCumAgy, agent.RunOptions{Timeout: time.Minute, SessionID: "old"})
	defer func() { cancel(); _ = wait() }()
	e1, err := agent.SendLive(context.Background(), id, "a", wire.Discard)
	if err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if e1.Usage != nil {
		t.Errorf("first resumed turn usage = %+v, want none (unknown), not the history", e1.Usage)
	}
	e2, err := agent.SendLive(context.Background(), id, "b", wire.Discard)
	if err != nil {
		t.Fatalf("second turn: %v", err)
	}
	if e2.Usage == nil || e2.Usage.InputTokens != 10 {
		t.Errorf("second turn usage = %+v, want its own 10", e2.Usage)
	}
}

// fakeCumAgy reports cumulative usage that starts at 1000 (a resumed conversation) and grows 10 per turn.
const fakeCumAgy = `#!/bin/sh
n=1000
while IFS= read -r line; do
  n=$((n + 10))
  printf '{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"r","usage":{"input_tokens":%d,"output_tokens":1}}}\n' "$n"
done
`
