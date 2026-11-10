package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/providers"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

// funcName compares function VALUES by the symbol they point to (PermissionHandlerFunc values are not
// comparable with ==); it tells approveToolOnce apart from denyAllTools.
func funcName(f any) string {
	return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
}

// EnableMemory turns into SessionConfig.Memory only when set, and permissionHandler follows AllowAllTools
// alone — a session (new or resumed) must carry both consistently.
func TestBuildSessionConfigMemoryAndPermission(t *testing.T) {
	cfg := BuildSessionConfig(SDKOptions{EnableMemory: true, AllowAllTools: true, Autonomous: true})
	if cfg.Memory == nil || !cfg.Memory.Enabled {
		t.Errorf("Memory = %+v, want enabled", cfg.Memory)
	}
	if funcName(cfg.OnPermissionRequest) != funcName(approveToolOnce) {
		t.Errorf("OnPermissionRequest = %s, want approveToolOnce (AllowAllTools+Autonomous)", funcName(cfg.OnPermissionRequest))
	}

	off := BuildSessionConfig(SDKOptions{})
	if off.Memory != nil {
		t.Errorf("Memory = %+v, want nil (EnableMemory=false)", off.Memory)
	}
	if funcName(off.OnPermissionRequest) != funcName(denyAllTools) {
		t.Errorf("OnPermissionRequest = %s, want denyAllTools (AllowAllTools=false)", funcName(off.OnPermissionRequest))
	}

	resumed := buildResumeSessionConfig(SDKOptions{EnableMemory: true})
	if resumed.Memory == nil || !resumed.Memory.Enabled {
		t.Errorf("resumed Memory = %+v, want enabled (mirrors BuildSessionConfig)", resumed.Memory)
	}
}

// agent.copilotWire blanks each BYOK credential var, then appends the real one: the session must get the last value.
func TestProviderFromEnvLastDuplicateWins(t *testing.T) {
	p := providerFromEnv([]string{
		"COPILOT_PROVIDER_BASE_URL=https://h/serving-endpoints", "COPILOT_PROVIDER_TYPE=openai",
		"COPILOT_PROVIDER_API_KEY=", "COPILOT_PROVIDER_BEARER_TOKEN=", "COPILOT_PROVIDER_BEARER_TOKEN=tok",
	})
	if p == nil || p.BearerToken != "tok" || p.APIKey != "" {
		t.Fatalf("provider = %+v, want BearerToken tok and no APIKey", p)
	}
}

func TestProviderFromEnvHeaders(t *testing.T) {
	p := providerFromEnv([]string{
		"COPILOT_PROVIDER_BASE_URL=https://h/serving-endpoints",
		"COPILOT_PROVIDER_HEADERS=X-Route: serving\nX-Context: tenant: workspace",
	})
	if p == nil {
		t.Fatal("provider = nil, want BYOK provider")
	}
	want := map[string]string{"X-Route": "serving", "X-Context": "tenant: workspace"}
	if !reflect.DeepEqual(p.Headers, want) {
		t.Errorf("Headers = %v, want %v", p.Headers, want)
	}
}

// A gateway Claude id keeps its wire name but gets copilot's catalog ModelID; other ids pass through.
func TestProviderFromEnvWellKnownModelID(t *testing.T) {
	for id, want := range map[string]string{
		"databricks-claude-sonnet-5-5": "claude-sonnet-5.5",
		"databricks-claude-opus-5-5":   "claude-opus-5.5",
		"claude-opus-5.5":              "claude-opus-5.5",
		"gpt-5":                        "gpt-5",
	} {
		p := providerFromEnv([]string{"COPILOT_PROVIDER_BASE_URL=https://h", "COPILOT_MODEL=" + id})
		if p.ModelID != want || p.WireModel != id {
			t.Errorf("%s: ModelID=%q WireModel=%q, want %q / %q", id, p.ModelID, p.WireModel, want, id)
		}
	}
}

// An explicit MODEL_ID and WIRE_API from the launcher win over the well-known mapping.
func TestProviderFromEnvResponsesWire(t *testing.T) {
	p := providerFromEnv([]string{
		"COPILOT_PROVIDER_BASE_URL=http://r/p/dev/responses", "COPILOT_PROVIDER_WIRE_API=responses",
		"COPILOT_PROVIDER_MODEL_ID=x", "COPILOT_MODEL=databricks-gpt-6-1-sol",
	})
	if p == nil || p.WireAPI != "responses" || p.ModelID != "x" {
		t.Fatalf("provider = %+v, want WireAPI responses and ModelID x", p)
	}
}

// stubListModels swaps listModelsFn for one returning models, restored when the test ends.
func stubListModels(t *testing.T, models []copilot.ModelInfo) {
	t.Helper()
	old := listModelsFn
	t.Cleanup(func() { listModelsFn = old })
	listModelsFn = func(ctx context.Context, o SDKOptions) ([]copilot.ModelInfo, error) { return models, nil }
}

// listStubbedModels runs SDKListModels against the stub and requires want models back.
func listStubbedModels(t *testing.T, want int) []SDKModel {
	t.Helper()
	got, err := SDKListModels(context.Background(), SDKOptions{CLIPath: "/fake/copilot"})
	if err != nil {
		t.Fatalf("SDKListModels: %v", err)
	}
	if len(got) != want {
		t.Fatalf("got %d models, want %d", len(got), want)
	}
	return got
}

// fieldsFixture is a two-model raw catalog: one with vision and billing, one with reasoning effort and none.
func fieldsFixture(mult *float64) []copilot.ModelInfo {
	return []copilot.ModelInfo{
		{
			ID:   "claude-sonnet-5",
			Name: "Claude Sonnet 5",
			Capabilities: copilot.ModelCapabilities{
				Supports: copilot.ModelSupports{Vision: true, ReasoningEffort: false},
			},
			Billing: &copilot.ModelBilling{Multiplier: mult},
		},
		{
			ID:   "gpt-5-mini",
			Name: "GPT-5 mini",
			Capabilities: copilot.ModelCapabilities{
				Supports: copilot.ModelSupports{Vision: false, ReasoningEffort: true},
			},
		},
	}
}

// ListModels maps the SDK's raw catalog into the harness's own Model shape, field for field.
func TestSDKListModelsMapsFields(t *testing.T) {
	mult := 2.5
	stubListModels(t, fieldsFixture(&mult))
	got := listStubbedModels(t, 2)
	t.Run("sonnet fields", func(t *testing.T) { checkSonnetFields(t, got[0], mult) })
	t.Run("mini fields", func(t *testing.T) { checkMiniFields(t, got[1]) })
}

func checkSonnetFields(t *testing.T, m SDKModel, mult float64) {
	t.Helper()
	if m.ID != "claude-sonnet-5" || m.Name != "Claude Sonnet 5" || !m.Vision || m.ReasoningEffort {
		t.Errorf("model[0] = %+v", m)
	}
	if m.Multiplier == nil || *m.Multiplier != mult {
		t.Errorf("model[0].Multiplier = %v, want %v", m.Multiplier, mult)
	}
}

func checkMiniFields(t *testing.T, m SDKModel) {
	t.Helper()
	if m.ID != "gpt-5-mini" || m.Vision || !m.ReasoningEffort {
		t.Errorf("model[1] = %+v", m)
	}
	if m.Multiplier != nil {
		t.Errorf("model[1].Multiplier = %v, want nil (no billing block)", *m.Multiplier)
	}
}

// policyFixture is a four-model raw catalog covering efforts, context limit and each Policy.State shape.
func policyFixture(ctxTokens *int) []copilot.ModelInfo {
	return []copilot.ModelInfo{
		{ // no Policy at all: enabled by default
			ID: "claude-sonnet-5",
			Capabilities: copilot.ModelCapabilities{
				Limits: copilot.ModelLimits{MaxContextWindowTokens: ctxTokens},
			},
			SupportedReasoningEfforts: []string{"low", "medium", "high"},
			DefaultReasoningEffort:    "medium",
		},
		{ // explicit Policy.State == "enabled"
			ID:     "gpt-5-mini",
			Policy: &copilot.ModelPolicy{State: "enabled"},
		},
		{ // disabled by policy
			ID:     "retired-model",
			Policy: &copilot.ModelPolicy{State: "disabled"},
		},
		{ // no context limit reported at all
			ID: "auto",
		},
	}
}

// SDKListModels also maps SupportedReasoningEfforts/DefaultReasoningEffort, MaxContextWindowTokens, and the
// Policy.State enabled gate — Policy nil and Policy.State=="enabled" both count as enabled.
func TestSDKListModelsMapsEffortsContextAndEnabled(t *testing.T) {
	ctxTokens := 1_000_000
	stubListModels(t, policyFixture(&ctxTokens))
	got := listStubbedModels(t, 4)
	t.Run("efforts and context", func(t *testing.T) {
		sonnet := got[0]
		if want := []string{"low", "medium", "high"}; !reflect.DeepEqual(sonnet.Efforts, want) {
			t.Errorf("sonnet Efforts = %v, want %v", sonnet.Efforts, want)
		}
		if sonnet.DefaultEffort != "medium" {
			t.Errorf("sonnet DefaultEffort = %q, want medium", sonnet.DefaultEffort)
		}
		if sonnet.ContextTokens != ctxTokens {
			t.Errorf("sonnet ContextTokens = %d, want %d", sonnet.ContextTokens, ctxTokens)
		}
	})
	t.Run("enabled gate", func(t *testing.T) {
		if !got[0].Enabled {
			t.Error("sonnet (no Policy) Enabled = false, want true (nil policy means enabled)")
		}
		if !got[1].Enabled {
			t.Error(`gpt-5-mini (Policy.State=="enabled") Enabled = false, want true`)
		}
		if got[2].Enabled {
			t.Error(`retired-model (Policy.State=="disabled") Enabled = true, want false`)
		}
	})
	t.Run("no context limit", func(t *testing.T) {
		if got[3].ContextTokens != 0 {
			t.Errorf("auto ContextTokens = %d, want 0 (no limit reported)", got[3].ContextTokens)
		}
	})
}

// CatalogOptions prefers an explicit token (GitHubToken) over UseLoggedInUser whenever the token is
// non-empty; an empty token falls back to the CLI's own stored login.
func TestCatalogOptionsPrefersToken(t *testing.T) {
	withToken := CatalogOptions("/fake/copilot", "gh-token-123")
	if withToken.GitHubToken != "gh-token-123" {
		t.Errorf("GitHubToken = %q, want gh-token-123", withToken.GitHubToken)
	}
	if withToken.UseLoggedInUser {
		t.Error("UseLoggedInUser = true with a token present, want false (token wins)")
	}
	noToken := CatalogOptions("/fake/copilot", "")
	if noToken.GitHubToken != "" {
		t.Errorf("GitHubToken = %q, want empty", noToken.GitHubToken)
	}
	if !noToken.UseLoggedInUser {
		t.Error("UseLoggedInUser = false with no token, want true (fall back to stored login)")
	}
}

// A lister failure (no login, CLI missing, RPC error) surfaces as a wrapped error naming the fix.
func TestSDKListModelsWrapsListerError(t *testing.T) {
	old := listModelsFn
	defer func() { listModelsFn = old }()
	wantErr := errors.New("rpc: not authenticated")
	listModelsFn = func(ctx context.Context, o SDKOptions) ([]copilot.ModelInfo, error) {
		return nil, wantErr
	}
	_, err := SDKListModels(context.Background(), SDKOptions{CLIPath: "/fake/copilot"})
	if err == nil {
		t.Fatal("SDKListModels: want error, got nil")
	}
	if !strings.Contains(err.Error(), "sign in with `copilot`") {
		t.Errorf("error %q does not name the fix", err.Error())
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("error %v does not wrap the lister's error %v", err, wantErr)
	}
}

// An empty catalog is not an error: SDKListModels returns an empty, non-nil slice.
func TestSDKListModelsEmptyCatalog(t *testing.T) {
	old := listModelsFn
	defer func() { listModelsFn = old }()
	listModelsFn = func(ctx context.Context, o SDKOptions) ([]copilot.ModelInfo, error) {
		return []copilot.ModelInfo{}, nil
	}
	got, err := SDKListModels(context.Background(), SDKOptions{CLIPath: "/fake/copilot"})
	if err != nil {
		t.Fatalf("SDKListModels: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d models, want 0", len(got))
	}
}

func TestFileAttachments(t *testing.T) {
	if got := fileAttachments(nil); len(got) != 0 {
		t.Errorf("no images must give no attachments, got %v", got)
	}
	got := fileAttachments([]string{"/tmp/a/shot.png"})
	f, ok := got[0].(*copilot.AttachmentFile)
	if len(got) != 1 || !ok || f.Path != "/tmp/a/shot.png" || f.DisplayName != "shot.png" {
		t.Fatalf("attachments = %#v", got)
	}
}

type fakeFleet struct {
	got *rpc.FleetStartRequest
	res *rpc.FleetStartResult
}

func (f *fakeFleet) Start(_ context.Context, p *rpc.FleetStartRequest) (*rpc.FleetStartResult, error) {
	f.got = p
	return f.res, nil
}

func TestStartFleetSendsPromptAndWaits(t *testing.T) {
	f := &fakeFleet{res: &rpc.FleetStartResult{Started: true}}
	if err := startFleet(context.Background(), f, copilot.MessageOptions{Prompt: "do it", Attachments: fileAttachments([]string{"/a.png"})}); err != nil {
		t.Fatal(err)
	}
	if f.got == nil || *f.got.Prompt != "do it" || f.got.Wait == nil || !*f.got.Wait || len(f.got.Attachments) != 1 {
		t.Fatalf("request = %+v", f.got)
	}
	f.res = &rpc.FleetStartResult{}
	if err := startFleet(context.Background(), f, copilot.MessageOptions{Prompt: "x"}); err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatalf("Started=false must error, got %v", err)
	}
}

// Usage events sum across calls; the SDK's inputTokens include cache reads/writes, which agent.Usage keeps apart.
func TestAssistantCollectorSumsUsage(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	var col assistantCollector
	col.onEvent(copilot.SessionEvent{Data: &copilot.AssistantUsageData{
		Model: "m", InputTokens: n(1000), OutputTokens: n(50), CacheReadTokens: n(600), CacheWriteTokens: n(100),
	}})
	col.onEvent(copilot.SessionEvent{Data: &copilot.AssistantMessageData{Content: " hi "}})
	col.onEvent(copilot.SessionEvent{Data: &copilot.AssistantUsageData{Model: "m", InputTokens: n(200), OutputTokens: n(25)}})
	col.onEvent(copilot.SessionEvent{Data: &copilot.AssistantUsageData{Model: "m"}})
	text, _, got := col.snapshot()
	want := wire.Usage{InputTokens: 500, CacheReadTokens: 600, CacheCreationTokens: 100, OutputTokens: 75}
	if got != want || text != "hi" {
		t.Fatalf("snapshot = %q, %+v; want hi, %+v", text, got, want)
	}
}

// Create and resume send the same append-mode system message and discovery roots; replace mode is never used.
func TestCreateAndResumeCarryInstructions(t *testing.T) {
	dir := t.TempDir()
	fields := func(session string) map[string]any {
		log := t.TempDir() + "/log"
		o := registryOpts(t, "1.0.104", log, registryPick)
		o.SessionID, o.System = session, "be brief"
		o.InstructionDirs, o.SkillDirs, o.AgentDirs = []string{dir}, []string{dir + "/skills"}, []string{dir + "/agents"}
		if _, err := openSession(t, o); err != nil {
			t.Fatal(err)
		}
		for _, c := range calls(t, log) {
			if _, params, ok := strings.Cut(c, " "); ok && (strings.HasPrefix(c, "session.create ") || strings.HasPrefix(c, "session.resume ")) {
				var m map[string]any
				if err := json.Unmarshal([]byte(params), &m); err != nil {
					t.Fatal(err)
				}
				return map[string]any{"sys": m["systemMessage"], "ins": m["instructionDirectories"], "skl": m["skillDirectories"], "agt": m["agentDirectories"]}
			}
		}
		t.Fatal("no session.create/resume recorded")
		return nil
	}
	want := map[string]any{
		"sys": map[string]any{"mode": "append", "content": "be brief"},
		"ins": []any{dir}, "skl": []any{dir + "/skills"}, "agt": []any{dir + "/agents"},
	}
	for name, got := range map[string]map[string]any{"create": fields(""), "resume": fields("sess-1")} {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s fields = %v, want %v", name, got, want)
		}
	}
}

// Copilot carries system text natively: both caps are Native (so the prompt stays unfolded) and System is filled.
func TestSystemPromptRidesSDKNotPrompt(t *testing.T) {
	if row.CLI.Caps.SystemPrompt != agent.Native || row.CLI.Caps.AppendSystemPrompt != agent.Native {
		t.Fatalf("caps = %v/%v, want Native/Native", row.CLI.Caps.SystemPrompt, row.CLI.Caps.AppendSystemPrompt)
	}
	req := agent.RunOptions{Prompt: "say hi", SystemPrompt: "be ZEBRA", AppendSystemPrompt: "end with ZEBRA"}
	o := newSDKOptions("copilot", nil, req, providers.Target{})
	if o.System != "be ZEBRA\n\nend with ZEBRA" {
		t.Errorf("System = %q, want both texts", o.System)
	}
	if cfg := BuildSessionConfig(o); cfg.SystemMessage == nil || cfg.SystemMessage.Mode != "append" {
		t.Errorf("SystemMessage = %+v, want append mode", cfg.SystemMessage)
	}
}

var (
	fakeCLIOnce sync.Once
	fakeCLIDir  string
	fakeCLIExe  string
	fakeCLIErr  error
)

// buildFakeCLI compiles the hermetic fake copilot CLI once per package; TestMain removes its dir.
func buildFakeCLI(t *testing.T) string {
	t.Helper()
	fakeCLIOnce.Do(func() {
		fakeCLIDir, fakeCLIErr = os.MkdirTemp("", "copilot-fakecli-")
		if fakeCLIErr != nil {
			return
		}
		fakeCLIExe = filepath.Join(fakeCLIDir, "fake")
		if out, err := exec.Command("go", "build", "-o", fakeCLIExe, "./testdata/fakecli").CombinedOutput(); err != nil {
			fakeCLIErr = fmt.Errorf("build fakecli: %w\n%s", err, out)
		}
	})
	if fakeCLIErr != nil {
		t.Fatal(fakeCLIErr)
	}
	return fakeCLIExe
}

func removeFakeCLI() {
	if fakeCLIDir != "" {
		_ = os.RemoveAll(fakeCLIDir)
	}
}

const registryPick = "dbx-openai/gpt-5-5"

// registryOpts is a valid registry-route SDKOptions aimed at the fake CLI.
func registryOpts(t *testing.T, version, log string, known ...string) SDKOptions {
	t.Helper()
	return SDKOptions{
		CLIPath: buildFakeCLI(t), Model: registryPick,
		Providers: []CopilotRegistryProvider{{Name: "dbx-openai", Type: "openai"}},
		Models:    []CopilotRegistryModel{{ID: "gpt-5-5", Provider: "dbx-openai"}},
		Token:     func() (string, error) { return "tok", nil },
		ChildEnv: []string{"FAKECLI_VERSION=" + version, "FAKECLI_RECORD=" + log,
			"FAKECLI_MODELS=" + strings.Join(known, ",")},
	}
}

func openSession(t *testing.T, o SDKOptions) (*copilot.Session, error) {
	t.Helper()
	co, err := BuildClientOptions(context.Background(), o)
	if err != nil {
		return nil, err
	}
	client := copilot.NewClient(co)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	if err := client.Start(ctx); err != nil {
		t.Fatalf("fake CLI did not start: %v", err)
	}
	t.Cleanup(func() { _ = client.Stop() })
	return newOrResumedSession(ctx, client, o, nil)
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	b, _ := os.ReadFile(log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// A pick outside the registry is refused with ErrUnregisteredPick, before any process starts (SDKOptions check) and
// when the CLI itself rejects the switch (so the pick can never silently stay on a GitHub model).
func TestRegistryUnregisteredPick(t *testing.T) {
	o := registryOpts(t, "1.0.104", t.TempDir()+"/log")
	o.Model = "dbx-openai/not-registered"
	if _, err := BuildClientOptions(context.Background(), o); !errors.Is(err, ErrUnregisteredPick) {
		t.Fatalf("BuildClientOptions err = %v, want ErrUnregisteredPick", err)
	}

	log := t.TempDir() + "/log"
	_, err := openSession(t, registryOpts(t, "1.0.104", log /* the CLI knows no model */))
	if !errors.Is(err, ErrUnregisteredPick) {
		t.Fatalf("session err = %v, want ErrUnregisteredPick from the CLI's rejection", err)
	}
}

// A resumed session is re-pinned through SetModel before any turn (it may carry another wire's saved model).
func TestRegistryResumeRepins(t *testing.T) {
	log := t.TempDir() + "/log"
	o := registryOpts(t, "1.0.104", log, registryPick)
	o.SessionID = "sess-1"
	s, err := openSession(t, o)
	if err != nil {
		t.Fatal(err)
	}
	_ = s
	got := calls(t, log)
	var resume, switched = -1, -1
	for i, c := range got {
		switch {
		case strings.HasPrefix(c, "session.resume "):
			resume = i
		case strings.HasPrefix(c, "session.model.switchTo ") && strings.Contains(c, `"modelId":"`+registryPick+`"`):
			switched = i
		}
	}
	if resume < 0 || switched < resume {
		t.Errorf("calls = %q; want session.resume followed by switchTo %s", got, registryPick)
	}
}

// A CLI older than MinMultiProviderCLI is refused with an explicit error before any session is made.
func TestRegistryProbeBelowMinimum(t *testing.T) {
	log := t.TempDir() + "/log"
	_, err := openSession(t, registryOpts(t, "1.0.103", log, registryPick))
	if !errors.Is(err, ErrNoMultiProvider) || !strings.Contains(err.Error(), MinMultiProviderCLI) {
		t.Fatalf("err = %v, want ErrNoMultiProvider naming %s", err, MinMultiProviderCLI)
	}
	for _, c := range calls(t, log) {
		if strings.HasPrefix(c, "session.create") || strings.HasPrefix(c, "session.resume") {
			t.Errorf("a session was made on an unsupported CLI: %q", c)
		}
	}
	if _, err := openSession(t, registryOpts(t, "1.0.104", t.TempDir()+"/log", registryPick)); err != nil {
		t.Errorf("exactly the minimum was refused: %v", err)
	}
}

// The fake CLI answers a send with an assistant message and records the session config it was created with.
func TestFakeCLISendEmitsReply(t *testing.T) {
	log := t.TempDir() + "/log"
	s, err := openSession(t, registryOpts(t, "1.0.104", log, registryPick))
	if err != nil {
		t.Fatal(err)
	}
	idle := make(chan struct{})
	var reply string
	s.On(func(ev copilot.SessionEvent) {
		switch d := ev.Data.(type) {
		case *copilot.AssistantMessageData:
			reply = d.Content
		case *copilot.SessionIdleData:
			close(idle)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.Send(ctx, copilot.MessageOptions{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-idle:
	case <-ctx.Done():
		t.Fatal("no idle event")
	}
	if reply != "fake reply" {
		t.Errorf("reply = %q, want %q", reply, "fake reply")
	}
	if got := strings.Join(calls(t, log), "\n"); !strings.Contains(got, "session.create ") {
		t.Errorf("record = %q, want session.create params", got)
	}
}

// A ctx cancel must kill the runtime startClient started, without the caller's stop func ever running.
func TestStartClientForceStopsOnCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a sh fake CLI")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	script := `#!/bin/sh
echo $$ > "` + pidFile + `"
while IFS= read -r line; do
  case "$line" in Content-Length:*) n=${line#*: }; n=${n%?};; esac
  [ -z "${line%?}" ] || continue
  body=$(head -c "$n")
  printf "%s\n" "$body" >> "` + pidFile + `.log"
  id=$(printf '%s' "$body" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  r='{"jsonrpc":"2.0","id":"'$id'","result":{"protocolVersion":` + strconv.Itoa(copilot.GetSDKProtocolVersion()) + `}}'
  printf 'Content-Length: %s\r\n\r\n%s' "${#r}" "$r"
done
`
	cli := filepath.Join(dir, "fake-copilot")
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stopErr error
	_, stop, err := startClient(ctx, SDKOptions{CLIPath: cli}, &stopErr)
	if err != nil {
		t.Fatalf("startClient: %v", err)
	}
	defer stop()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("pid file = %q, want the runtime pid: %v", raw, err)
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("runtime pid %d still alive after ctx cancel", pid)
	}
}
