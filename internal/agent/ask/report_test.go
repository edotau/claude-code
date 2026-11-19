package ask

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// Text keeps ran-on and the dispatch id, json drops both, a refusal drops did-not-run.
func TestAskTrailerByFormatAndRefusal(t *testing.T) {
	ran := wire.Result{DispatchID: "d1", Agent: "claude", Status: wire.StatusOK, Hops: []wire.Hop{{Agent: "claude"}}}
	failed := wire.Result{DispatchID: "d2", Agent: "claude", Status: wire.StatusError, FallbackReason: "claude: no login"}
	if wire.RanOn(ran) == "" || wire.DidNotRun(failed) == "" {
		t.Fatal("fixtures must produce a ran-on and a did-not-run line")
	}
	for _, tc := range []struct {
		name    string
		format  string
		res     wire.Result
		refused bool
		want    []string
	}{
		{"text ran", helptext.Text, ran, false, []string{wire.RanOn(ran), "", "dispatch d1"}},
		{"json failed", helptext.JSON, failed, false, []string{"", wire.DidNotRun(failed), ""}},
		{"text refused", helptext.Text, failed, true, []string{wire.RanOn(failed), "", "dispatch d2"}},
	} {
		if got := askTrailer(tc.format, tc.res, tc.refused); !slices.Equal(got, tc.want) {
			t.Errorf("%s: askTrailer = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestReportSandboxDenialsTextOnly(t *testing.T) {
	const summary = "sandbox refused 3 commands: go test, python3 -c, ps…; re-run outside the sandbox\n"
	for _, format := range []string{helptext.Text, helptext.JSON, agent.FormatStreamJSON} {
		for _, d := range []*wire.Denials{nil, {Count: 3, Commands: []string{"go test", "python3 -c", "ps aux"}}} {
			res := wire.Result{Answer: `{"ok":true}`, Structured: []byte(`{"ok":true}`), Denials: d}
			var out bytes.Buffer
			if err := reportAskResult(&out, format, res, nil); err != nil {
				t.Fatal(err)
			}
			want := "\n"
			if d != nil {
				want += summary
			}
			if format == helptext.Text {
				var saved bytes.Buffer
				if err := reportSaved(&saved, format, "", res, errors.New("run failed")); err != nil {
					t.Fatal(err)
				}
				if saved.String() != res.Answer+want {
					t.Fatalf("saved text = %q", saved.String())
				}
			}
			if format == helptext.Text && out.String() != want {
				t.Fatalf("text = %q, want %q", out.String(), want)
			}
			if format != helptext.Text && strings.Contains(out.String(), "sandbox refused") {
				t.Fatalf("summary in %s: %s", format, &out)
			}
			if res.Answer != `{"ok":true}` || string(res.Structured) != `{"ok":true}` {
				t.Fatal("answer changed")
			}
		}
	}
}

func TestPrintDenialsFitsEightyColumns(t *testing.T) {
	const tail = "; re-run outside the sandbox"
	for _, c := range []struct {
		d    wire.Denials
		want string
	}{
		{wire.Denials{Count: 1, Commands: []string{"go env"}}, "sandbox refused 1 command: go env" + tail},
		{wire.Denials{Count: 12, Commands: []string{"go test", "python3 -c", "ps aux"}}, "sandbox refused 12 commands: go test, python3 -c, p…" + tail},
		{wire.Denials{Count: 3, Commands: []string{strings.Repeat("界", 40)}}, "sandbox refused 3 commands: " + strings.Repeat("界", 11) + "…" + tail},
	} {
		var out bytes.Buffer
		if err := printDenials(&out, wire.Result{Denials: &c.d}); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSuffix(out.String(), "\n"); got != c.want || textutil.DisplayWidth(got) > 80 {
			t.Errorf("summary = %q (%d cells), want %q", got, textutil.DisplayWidth(got), c.want)
		}
	}
}

func TestPrintDenialsWithoutLabels(t *testing.T) {
	var out bytes.Buffer
	if err := printDenials(&out, wire.Result{Denials: &wire.Denials{Count: 2}}); err != nil {
		t.Fatal(err)
	}
	if want := "sandbox refused 2 commands; re-run outside the sandbox\n"; out.String() != want {
		t.Errorf("summary = %q, want %q", out.String(), want)
	}
}

// A --job run writes its answer to the log through stdout; a vault value in it is masked there too.
func TestCmdAskJobLogMasksVaultValueInAnswer(t *testing.T) {
	redact.StubValues(t, []string{"answer-alpha"})
	agyJobFixture(t)
	got := runAskJobIntoLog(t)
	if strings.Contains(got, "answer-alpha") || !strings.Contains(got, "[redacted]") {
		t.Errorf("log must mask the vault value in every line:\n%s", got)
	}
}

func fakeAskOutput(t *testing.T, answer string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv(paths.AgentBinDirEnv, paths.BinDir())
	body, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	pathstest.FakeNativeClaude(t, "cat > /dev/null\n"+
		`printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":`+string(body)+`}]}}'`+"\n"+
		`printf '%s\n' '{"type":"result","subtype":"success","result":`+string(body)+`}'`)
}

func TestAskSaveMasksAnswer(t *testing.T) {
	skipWithoutSh(t)
	const fake = `fake"ask\value-123`
	fakeAskOutput(t, "answer: "+fake)
	redact.StubValues(t, []string{fake})
	path := filepath.Join(t.TempDir(), "answer.txt")
	code, stdout, stderr := hosttest.CaptureOutput(t, func() int {
		return Cmd([]string{"--agent", "claude", "--fallback", "none", "--format", "json", "--save", path, "test"})
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0; %s", code, strings.ReplaceAll(stderr, fake, "[redacted]"))
	}
	want := redact.Text("answer: " + fake)
	if got := pathstest.ReadFile(t, path); got != want {
		t.Error("saved answer was not masked")
	}
	var env wire.Envelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || env.Answer != want {
		t.Error("stdout did not contain a valid JSON envelope with a masked answer")
	}
}

type askFlushFailure struct{ writes int }

func (w *askFlushFailure) Write([]byte) (int, error) {
	w.writes++
	return 0, errors.New("fake write failure")
}

func TestAskFinalFlushFailure(t *testing.T) {
	skipWithoutSh(t)
	fakeAskOutput(t, "ok")
	redact.StubValues(t, []string{"fake-unrelated-value-123"})
	var out askFlushFailure
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		return cmdTo([]string{"--agent", "claude", "--fallback", "none", "test"}, &out)
	})
	if out.writes != 1 {
		t.Fatalf("destination writes = %d, want one final flush; %s", out.writes, stderr)
	}
	const line = "claude-code: ask: flush output: fake write failure\n"
	if code != 1 || strings.Count(stderr, line) != 1 {
		t.Errorf("exit = %d, flush diagnostics = %d; want 1 and 1", code, strings.Count(stderr, line))
	}
}

// Live send, stored result and the memory sink all mask a vault value.
func TestPrintedEnvelopesMaskSecrets(t *testing.T) {
	const fake = "fake-vault-value-ask-13c"
	redact.StubValues(t, []string{fake})
	env := wire.Envelope{Answer: "a " + fake}
	_, out, _ := hosttest.CaptureOutput(t, func() int {
		if err := printLiveEnvelope(env, helptext.JSON); err != nil {
			t.Error(err)
		}
		return 0
	})
	if strings.Contains(out, fake) || !strings.Contains(out, "[redacted]") {
		t.Errorf("printLiveEnvelope = %q, want the value masked", out)
	}

	jobsEnv(t)
	rec := seedJob(t, time.Minute, "error", "log "+fake+"\n", 0)
	rec.Error = "boom " + fake
	for _, format := range []string{helptext.JSON, helptext.Text} {
		_, out, stderr := hosttest.CaptureOutput(t, func() int { return emitResult("agents result", rec, "log "+fake+"\n", format) })
		if strings.Contains(out, fake) || strings.Contains(stderr, fake) {
			t.Errorf("emitResult %s printed the value: %q", format, out)
		}
	}

	var b bytes.Buffer
	if err := memorySink(&b).Emit(wire.Event{Type: wire.EventText, Text: "x " + fake}); err != nil || strings.Contains(b.String(), fake) {
		t.Errorf("memory sink = %q, %v, want the value masked", b.String(), err)
	}
}

// A structured payload with no secret passes through byte for byte, key order and number formatting included.
func TestMaskStructuredKeepsCleanBytes(t *testing.T) {
	raw := json.RawMessage(`{"z":1.50,"a":[1e3,"x"]}`)
	if got := maskStructured(raw); string(got) != string(raw) {
		t.Errorf("maskStructured = %s, want %s unchanged", got, raw)
	}
}
