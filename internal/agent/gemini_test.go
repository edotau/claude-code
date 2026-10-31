package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// agy has no custom base URL, so a gateway provider for the gemini vendor must fail fast with a clear error.
func TestGeminiGateway(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GW_KEY", secret)
	p := &providers.Provider{Name: "gw", Protocol: providers.ProtocolGemini, BaseURL: "https://gw.example/gemini",
		Auth: providers.Auth{Type: providers.AuthKey, Env: "GW_KEY"}, Headers: map[string]string{"X-Team": "a"}}
	_, _, err := schemaFor("gemini").Configure(context.Background(), providers.Target{Provider: p, Model: "m"}, "")
	if err == nil || !strings.Contains(err.Error(), "no custom base URL") {
		t.Fatalf("err = %v, want a clear \"no custom base URL\" error", err)
	}
}

// gemini runs only on agy's cached Google login (AgyLoginMarker under HOME): no marker refuses to
// configure (no GEMINI_API_KEY fallback), a marker on disk lets it through with no key injected.
func TestGeminiLoginPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if GeminiLoginPresent() {
		t.Fatal("no marker on disk must read as not logged in")
	}
	tg := target(t, "antigravity", "gemini-2.5-pro")
	// target() reset HOME to its own tempdir; point it back at ours for the marker check.
	t.Setenv("HOME", home)

	if _, _, err := schemaFor("gemini").configureHeadless(context.Background(), tg, Headless{Prompt: "hi"}); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Fatalf("without a login marker, err = %v, want a not-signed-in error", err)
	}

	marker := AgyLoginMarker(home)
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !GeminiLoginPresent() {
		t.Fatal("marker on disk must read as logged in")
	}

	env, _, err := schemaFor("gemini").configureHeadless(context.Background(), tg, Headless{Prompt: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := envMap(env)["GEMINI_API_KEY"]; ok {
		t.Errorf("with a login marker, GEMINI_API_KEY must not be set, got %q", got)
	}
}

// D6: agy's model is flag > process env > the GEMINI_MODEL vault pin > agy's default.
func TestAgyModelPrecedence(t *testing.T) {
	vaulttest.Use(t)
	for _, k := range modelEnvKeys {
		t.Setenv(k, "")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel, _ := providers.Select(reg, "")
	if got := geminiAgyModel(reg, sel, reg.Providers["anthropic"], ""); got != AgyDefaultModel {
		t.Errorf("nothing set = %q, want the default %q", got, AgyDefaultModel)
	}
	if err := providers.SetPins(map[string]string{providers.PinGeminiModel: "gemini-pinned"}); err != nil {
		t.Fatal(err)
	}
	if got := geminiAgyModel(reg, sel, reg.Providers["anthropic"], ""); got != "gemini-pinned" {
		t.Errorf("vault pin = %q, want gemini-pinned", got)
	}
	t.Setenv("GEMINI_MODEL", "gemini-env")
	if got := geminiAgyModel(reg, sel, reg.Providers["anthropic"], ""); got != "gemini-env" {
		t.Errorf("process env = %q, want it to beat the pin", got)
	}
	if got := geminiAgyModel(reg, sel, reg.Providers["anthropic"], "gemini-flag"); got != "gemini-flag" {
		t.Errorf("flag = %q, want it to beat env and pin", got)
	}
}

// Regression: no GEMINI_PROFILE pin and no Google login must refuse, never bill the session's Databricks row.
func TestResolveTargetGeminiNoLoginNoPinRefuses(t *testing.T) {
	vaulttest.Use(t)
	t.Setenv("HOME", t.TempDir())
	reg := &providers.Registry{Providers: map[string]*providers.Provider{}}
	p := dbxRow("qa", "databricks-")
	reg.Providers[p.Name] = p
	sel := providers.Selection{Provider: p, Slots: map[string]providers.Target{"opus": {Provider: p, Model: "databricks-claude-opus-5-5"}}}
	_, _, err := schemaFor("gemini").ResolveTarget(reg, sel, false)
	if err == nil || !strings.Contains(err.Error(), "GEMINI_PROFILE") {
		t.Fatalf("err = %v, want the Google-login-or-pin refusal", err)
	}
}

// Regression: --direct has no Databricks route for agy (the router swaps the token); refuse, don't route.
func TestGeminiGatewayRefusesDirect(t *testing.T) {
	vaulttest.Use(t)
	StubRouterForTest(t, "http://127.0.0.1:1")
	_, _, err := schemaFor("gemini").configureRoute(WithDirect(context.Background()),
		providers.Target{Provider: dbxRow("qa", "databricks-"), Model: "gemini-3.8-flash"}, &Headless{Prompt: "hi"})
	if err == nil || !strings.Contains(err.Error(), "--direct") {
		t.Fatalf("err = %v, want a --direct refusal", err)
	}
}

func TestParseAgyStreamResultApplication(t *testing.T) {
	const init = `{"event":"init","conversation_id":"init-id"}` + "\n"
	const delta = `{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":" partial "}}` + "\n"
	cases := []struct {
		name, stream, answer, output, session, err string
		usage                                      Usage
	}{
		{
			name:   "response fallback and result metadata",
			stream: init + `{"event":"result","result":{"status":"SUCCESS","conversation_id":"result-id","response":" answer \n","usage":{"input_tokens":7,"output_tokens":3,"thinking_tokens":2,"cache_read_tokens":4}}}`,
			answer: "answer", output: " answer \n", session: "result-id",
			usage: Usage{InputTokens: 7, OutputTokens: 5, CacheReadTokens: 4},
		},
		{
			name:   "deltas win and init session survives",
			stream: init + delta + `{"event":"result","result":{"status":"SUCCESS","response":"duplicate"}}`,
			answer: "partial", output: " partial ", session: "init-id",
		},
		{
			name:   "failure retains partial answer and usage",
			stream: init + delta + `{"event":"result","result":{"status":"ERROR","response":"not emitted","error":"upstream failed","usage":{"input_tokens":7,"thinking_tokens":2}}}`,
			answer: "partial", output: " partial ", session: "init-id", err: "upstream failed",
			usage: Usage{InputTokens: 7, OutputTokens: 2},
		},
		{
			name: "status error fallback", stream: `{"event":"result","result":{"status":"CANCELLED","response":"not emitted"}}`,
			err: "agy run CANCELLED",
		},
		{
			name: "missing status error fallback", stream: `{"event":"result","result":{}}`,
			err: "agy run failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			res, err := parseAgyStream(newBR(tc.stream), NewTextSink(&out), nil)
			if (err == nil) != (tc.err == "") || err != nil && err.Error() != tc.err {
				t.Fatalf("err = %v, want %q", err, tc.err)
			}
			if res.Answer != tc.answer || out.String() != tc.output || res.SessionID != tc.session || res.Usage != tc.usage {
				t.Fatalf("res = %+v, output = %q", res, out.String())
			}
		})
	}
}

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

type failingStreamWriter struct{}

func (failingStreamWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestParseAgyStreamWriteErrors(t *testing.T) {
	for _, stream := range []string{
		agyOK,
		`{"event":"result","result":{"status":"SUCCESS","response":"fallback"}}`,
	} {
		if _, err := parseAgyStream(newBR(stream), NewTextSink(failingStreamWriter{}), nil); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("err = %v, want closed pipe", err)
		}
	}
}

// truncateBeforeResult drops every line at-and-after the "result" event from a stream-json fixture,
// simulating a connection that dies mid-run (the agy binary itself was not reachable in this sandbox
// to capture a fresh truncated stream live, so this derives the truncation from the agyOK fixture
// above, itself captured live from `agy --output-format stream-json` 1.4.2).
func truncateBeforeResult(t *testing.T, stream string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stream, "\n"), "\n")
	var kept []string
	for _, l := range lines {
		if strings.Contains(l, `"event":"result"`) {
			break
		}
		kept = append(kept, l)
	}
	if len(kept) == len(lines) {
		t.Fatalf("fixture has no result event to truncate before: %q", stream)
	}
	return strings.Join(kept, "\n") + "\n"
}

// A stream that EOFs before any result event is a truncated connection, not a quiet success: gemini.go
// must surface it rather than returning a zero-Usage "success" with whatever text happened to stream.
func TestParseAgyStream_TruncatedNoResult(t *testing.T) {
	truncated := truncateBeforeResult(t, agyOK)
	var out strings.Builder
	res, err := parseAgyStream(newBR(truncated), NewTextSink(&out), nil)
	if err == nil || err.Error() != "agy: stream ended without a result event" {
		t.Fatalf("err = %v, want \"agy: stream ended without a result event\"", err)
	}
	// The step_update text that did arrive before truncation is still surfaced, not thrown away.
	if res.Answer != "pong" || out.String() != "pong" {
		t.Errorf("res.Answer = %q, streamed %q, want %q (the pre-truncation delta)", res.Answer, out.String(), "pong")
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

// TestParseAgyStreamPartial: a SUCCESS turn that ends on a backgrounded command or with no answer is partial.
func TestParseAgyStreamPartial(t *testing.T) {
	ok := `{"event":"result","result":{"status":"SUCCESS","response":""}}`
	step := func(typ, state string) string {
		return `{"event":"step_update","step_update":{"step_type":"` + typ + `","state":"` + state + `"}}` + "\n"
	}
	text := `{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"waiting"}}` + "\n"
	for name, stream := range map[string]string{
		"open command": text + step("run_command", "ACTIVE") + ok,
		"empty answer": step("user_input", "DONE") + ok,
	} {
		res, err := parseAgyStream(newBR(stream), NewTextSink(&strings.Builder{}), nil)
		var e *Error
		if !errors.As(err, &e) || !e.Progressed || statusOf(e) != StatusPartial {
			t.Errorf("%s: err = %v, want a partial-status error", name, err)
		}
		_ = res
	}
	done := text + step("run_command", "DONE") + `{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":" done"}}` + "\n" + ok
	if _, err := parseAgyStream(newBR(done), NewTextSink(&strings.Builder{}), nil); err != nil {
		t.Errorf("finished command then answer: err = %v, want nil", err)
	}
}

// TestParseAgyStreamOpenCommandSurvivesResponse: a response delta after an ACTIVE command does not close it.
func TestParseAgyStreamOpenCommandSurvivesResponse(t *testing.T) {
	step := func(state string) string {
		s := `"step_type":"run_command"`
		if state != "" {
			s += `,"state":"` + state + `"`
		}
		return `{"event":"step_update","step_update":{` + s + `}}` + "\n"
	}
	text := `{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"bg"}}` + "\n"
	ok := `{"event":"result","result":{"status":"SUCCESS"}}`
	_, err := parseAgyStream(newBR(step("ACTIVE")+text+ok), NewTextSink(&strings.Builder{}), nil)
	var e *Error
	if !errors.As(err, &e) || statusOf(e) != StatusPartial {
		t.Errorf("active command then response: err = %v, want partial", err)
	}
	if _, err := parseAgyStream(newBR(step("")+text+ok), NewTextSink(&strings.Builder{}), nil); err != nil {
		t.Errorf("state-less step: err = %v, want nil", err)
	}
}

func geminiCells(t *testing.T, pin string) []string {
	t.Helper()
	tb := testTable(t)
	vaulttest.Use(t)
	if pin != "" {
		if err := providers.SetPins(map[string]string{providers.PinGeminiModel: pin}); err != nil {
			t.Fatal(err)
		}
	}
	r := tb.Pick(context.Background(), "sonnet", "gemini", router.State{}, RosterPick(router.PickOpts{Configured: allConfigured, Now: func() time.Time { return now0 }}))
	var models []string
	for _, c := range r.Candidates {
		if c.Agent == "gemini" {
			models = append(models, c.Model)
		}
	}
	return models
}

// GEMINI_MODEL pin on the profile-less (native agy login) gemini leg beats the table cell.
func TestGeminiModelPinBeatsTableCellWithoutProfile(t *testing.T) {
	got := geminiCells(t, "gemini-pinned-x")
	if len(got) == 0 {
		t.Fatal("no gemini candidate")
	}
	for _, m := range got {
		if m != "gemini-pinned-x" {
			t.Errorf("gemini cell model = %q, want the pin", m)
		}
	}
}

// With no pin the table cell is kept.
func TestGeminiNoPinKeepsTableCell(t *testing.T) {
	got := geminiCells(t, "")
	if len(got) == 0 {
		t.Fatal("no gemini candidate")
	}
	for _, m := range got {
		if m == "" || m == "gemini-pinned-x" {
			t.Errorf("gemini cell model = %q, want the table cell", m)
		}
	}
}

func TestGeminiCanceledContext(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "package a")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, dirs := range [][]string{nil, {"."}} {
		got, err := CollectContext(ctx, dir, dirs, []string{"*.go"}, 40, 100)
		if !errors.Is(err, context.Canceled) || len(got.Included) != 0 {
			t.Errorf("CollectContext(%v) = %+v, %v, want cancellation without files", dirs, got, err)
		}
	}
	if _, err := ChangedFilesVsHEAD(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Errorf("ChangedFilesVsHEAD error = %v, want cancellation", err)
	}
	if _, err := diffFiles(ctx, GeminiOptions{Cwd: dir, Diff: true}); !errors.Is(err, context.Canceled) {
		t.Errorf("diffFiles error = %v, want cancellation", err)
	}
}

func TestGeminiGitCommandCancellation(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not on PATH")
	}
	for _, tc := range []struct {
		name  string
		stage string
		run   func(context.Context, string) (Context, error)
	}{
		{"collect probe", "rev-parse", func(ctx context.Context, dir string) (Context, error) {
			return CollectContext(ctx, dir, []string{"."}, nil, 40, 100)
		}},
		{"collect list", "ls-files", func(ctx context.Context, dir string) (Context, error) {
			return CollectContext(ctx, dir, []string{"."}, nil, 40, 100)
		}},
		{"changed diff", "diff", func(ctx context.Context, dir string) (Context, error) {
			_, err := ChangedFilesVsHEAD(ctx, dir)
			return Context{}, err
		}},
		{"changed list", "ls-files", func(ctx context.Context, dir string) (Context, error) {
			_, err := ChangedFilesVsHEAD(ctx, dir)
			return Context{}, err
		}},
		{"bridge diff", "diff", func(ctx context.Context, dir string) (Context, error) {
			_, err := diffFiles(ctx, GeminiOptions{Cwd: dir, Diff: true})
			return Context{}, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, bin := t.TempDir(), t.TempDir()
			marker := filepath.Join(dir, "git-started")
			write(t, filepath.Join(dir, ".gitignore"), "ignored.txt\n")
			write(t, filepath.Join(dir, "ignored.txt"), "must not be inlined")
			script := "#!/bin/sh\n" +
				"if [ \"$3\" = \"" + tc.stage + "\" ]; then\n" +
				"  : > \"$GEMINI_TEST_GIT_MARKER\"\n" +
				"  exec \"" + sleep + "\" 3\n" +
				"fi\n" +
				"case \"$3\" in\n" +
				"  rev-parse) printf 'true\\n' ;;\n" +
				"  diff) printf 'changed.go\\n' ;;\n" +
				"  ls-files) printf 'tracked.go\\000' ;;\n" +
				"  *) exit 1 ;;\n" +
				"esac\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			t.Setenv("GEMINI_TEST_GIT_MARKER", marker)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			type result struct {
				context Context
				err     error
			}
			done := make(chan result, 1)
			go func() {
				got, err := tc.run(ctx, dir)
				done <- result{got, err}
			}()
			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				select {
				case <-tick.C:
				case got := <-done:
					t.Fatalf("git returned before synchronization: %+v, %v", got.context, got.err)
				case <-deadline.C:
					t.Fatal("git did not reach the cancellation synchronization marker")
				}
			}
			canceledAt := time.Now()
			cancel()
			select {
			case got := <-done:
				if !errors.Is(got.err, context.Canceled) {
					t.Errorf("error = %v, want cancellation", got.err)
				}
				if len(got.context.Included) != 0 {
					t.Errorf("canceled scan included files: %+v", got.context.Included)
				}
				if elapsed := time.Since(canceledAt); elapsed > time.Second {
					t.Errorf("git took %s to stop after cancellation", elapsed)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("git command did not stop after cancellation")
			}
		})
	}
}

func TestCollectContextGitFailureFallback(t *testing.T) {
	for _, stage := range []string{"rev-parse", "ls-files"} {
		t.Run(stage, func(t *testing.T) {
			dir, bin := t.TempDir(), t.TempDir()
			write(t, filepath.Join(dir, "a.go"), "package a")
			script := "#!/bin/sh\nif [ \"$3\" = \"" + stage + "\" ]; then exit 1; fi\nprintf 'true\\n'\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			got, err := CollectContext(t.Context(), dir, []string{"."}, nil, 40, 100)
			if err != nil || len(got.Included) != 1 || got.Included[0].Path != "a.go" {
				t.Fatalf("CollectContext = %+v, %v, want ordinary git failure fallback", got, err)
			}
		})
	}
}

func TestChangedFilesCancellationMentionsGit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := ChangedFilesVsHEAD(ctx, t.TempDir())
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "git") {
		t.Fatalf("error = %v, want wrapped git cancellation", err)
	}
}

// A bridge dispatch on the agy backend leaves a 0600 <id>.log (record.Log) holding the redacted agy output.
func TestBridgeLogCapturesRedactedAgyOutput(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", loginHome(t, true))
	script := "cat > /dev/null\n" +
		"echo 'agy-stderr-line' >&2\n" +
		"echo 'export AWS_SECRET=AKIAIOSFODNN7EXAMPLE' >&2\n" +
		`printf '%s\n' '{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"bridge-answer","num_turns":1}}'` + "\n"
	pathstest.FakeVendor(t, "agy", script)

	res, err := RunGemini(context.Background(), GeminiOptions{Cwd: t.TempDir(), Task: "t"}, io.Discard, io.Discard)
	if err != nil || res.Answer != "bridge-answer" {
		t.Fatalf("Run = (%q, %v), want (bridge-answer, nil)", res.Answer, err)
	}
	recs := dispatchRecords(t)
	if len(recs) != 1 {
		t.Fatalf("dispatch records = %d, want 1", len(recs))
	}
	logPath := recs[0].Log
	if logPath == "" {
		t.Fatal("record.Log is empty: the bridge dispatch did not record a log path")
	}
	if logPath != recs[0].LogPath() {
		t.Errorf("record.Log = %q, want %q", logPath, recs[0].LogPath())
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("log file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log mode = %o, want 0600", perm)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{"agy-stderr-line", "bridge-answer", "AWS_SECRET=REDACTED"} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("log leaks an unredacted secret:\n%s", got)
	}
	if n := strings.Count(got, "agy-stderr-line"); n != 1 {
		t.Errorf("stderr line appears %d times, want exactly once:\n%s", n, got)
	}
}
