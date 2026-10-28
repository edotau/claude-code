package databricks

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
)

// call is one recorded spawn of the child.
type call struct {
	bin  string
	argv []string
	env  []string
}

// fakeChild records every spawn and answers the smoke and needle prompts from its script.
type fakeChild struct {
	calls  []call
	smoke  string // stdout for the smoke prompt
	needle string // stdout for the needle prompt
	code   int
	err    error
	body   []byte // stdin the needle prompt received
}

func (f *fakeChild) spawn(bin string, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.calls = append(f.calls, call{bin: bin, argv: slices.Clone(argv), env: slices.Clone(env)})
	if f.err != nil {
		return 0, f.err
	}
	prompt := argValue(argv, "-p")
	if prompt == needlePrompt {
		f.body, _ = io.ReadAll(stdin)
		_, _ = io.WriteString(stdout, needleJSON(f.needle))
		return f.code, nil
	}
	_, _ = io.WriteString(stdout, f.smoke)
	return f.code, nil
}

// needleJSON wraps a reply in claude's --output-format json shape with 300K input tokens.
func needleJSON(reply string) string {
	b, _ := json.Marshal(map[string]any{"result": reply, "usage": map[string]int{"input_tokens": 300000}})
	return string(b) + "\n"
}

// useFake swaps the spawn seam for f and restores the real one at cleanup.
func useFake(t *testing.T, f *fakeChild) {
	t.Helper()
	old := spawn
	spawn = f.spawn
	t.Cleanup(func() { spawn = old })
}

// argValue returns the element after flag in argv, or "".
func argValue(argv []string, flag string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

// runCaptured runs fn with os.Stdout and os.Stderr redirected and returns the exit code and both streams.
func runCaptured(t *testing.T, fn func() int) (int, string, string) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	t.Cleanup(func() { os.Stdout, os.Stderr = oldOut, oldErr })
	outCh, errCh := drain(outR), drain(errR)
	code := fn()
	_ = outW.Close()
	_ = errW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	return code, <-outCh, <-errCh
}

// drain reads r to EOF on a goroutine so a large write cannot block the pipe.
func drain(r *os.File) <-chan string {
	ch := make(chan string, 1)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		ch <- b.String()
	}()
	return ch
}

// TestPlanTable pins the default model, env selection, passthrough order and --env forms.
func TestPlanTable(t *testing.T) {
	seedOverlay(t, "") // the real vault's migrated dev/qa/prod rows would rename the provider
	tests := []struct {
		name     string
		args     []string
		wantProv string
		wantMod  string
		wantRest []string
	}{
		{"defaults", nil, "databricks-qa", "databricks-claude-opus-5-5[1m]", nil},
		{"model only", []string{"sonnet"}, "databricks-qa", "sonnet", nil},
		{"--env dev", []string{"--env", "dev"}, "databricks-dev", "databricks-claude-opus-5-5[1m]", nil},
		{"--env prod after flags", []string{"-p", "hi", "--env", "prod"}, "databricks-prod", "databricks-claude-opus-5-5[1m]", []string{"-p", "hi"}},
		{"--env= form", []string{"--env=dev"}, "databricks-dev", "databricks-claude-opus-5-5[1m]", nil},
		{"model + --env=prod", []string{"haiku", "--env=prod"}, "databricks-prod", "haiku", nil},
		{"rest order kept", []string{"--env", "prod", "ask", "hello world"}, "databricks-prod", "databricks-claude-opus-5-5[1m]", []string{"ask", "hello world"}},
		{"model + env + mixed", []string{"sonnet", "--env=qa", "-p", "test", "ask"}, "databricks-qa", "sonnet", []string{"-p", "test", "ask"}},
		{"flags-first keeps default model", []string{"-p", "hi"}, "databricks-qa", "databricks-claude-opus-5-5[1m]", []string{"-p", "hi"}},
		{"only MODEL is taken", []string{"sonnet", "haiku"}, "databricks-qa", "sonnet", []string{"haiku"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := plan(tt.args)
			if err != nil {
				t.Fatalf("plan(%q) error: %v", tt.args, err)
			}
			if p.Provider != tt.wantProv || p.Model != tt.wantMod {
				t.Errorf("got provider %q model %q, want %q %q", p.Provider, p.Model, tt.wantProv, tt.wantMod)
			}
			if !slices.Equal(p.Rest, tt.wantRest) {
				t.Errorf("rest = %q, want %q", p.Rest, tt.wantRest)
			}
		})
	}
}

// allEnvTwins defines a preset plus an -oai twin for each env, so the plan tests never read the real vault.
const allEnvTwins = `{"providers":{` +
	`"databricks-dev":{"preset":"databricks",` + dbxHost + `},"databricks-dev-oai":{"kind":"openai","base_url":"https://adb-1.azuredatabricks.net/serving-endpoints","auth":{"type":"none"}},` +
	`"databricks-qa":{"preset":"databricks",` + dbxHost + `},"databricks-qa-oai":{"kind":"openai","base_url":"https://adb-1.azuredatabricks.net/serving-endpoints","auth":{"type":"none"}},` +
	`"databricks-prod":{"preset":"databricks",` + dbxHost + `},"databricks-prod-oai":{"kind":"openai","base_url":"https://adb-1.azuredatabricks.net/serving-endpoints","auth":{"type":"none"}}}}`

// TestPlanRouting pins which model ids take the Anthropic passthrough versus the -oai router.
func TestPlanRouting(t *testing.T) {
	seedOverlay(t, allEnvTwins)
	passthrough := []string{"opus", "sonnet", "haiku", "databricks-claude-3-5-sonnet", "databricks-claude-opus-5-5[1m]"}
	router := []string{"databricks-kimi-k3", "glm-4", "gpt-4", "gemini-2.0"}
	for _, m := range passthrough {
		t.Run("passthrough "+m, func(t *testing.T) {
			p, err := plan([]string{m, "--env", "dev"})
			if err != nil {
				t.Fatal(err)
			}
			if p.Provider != "databricks-dev" {
				t.Errorf("provider = %q, want databricks-dev", p.Provider)
			}
		})
	}
	for _, m := range router {
		t.Run("router "+m, func(t *testing.T) {
			p, err := plan([]string{m, "--env", "prod"})
			if err != nil {
				t.Fatal(err)
			}
			if p.Provider != "databricks-prod-oai" {
				t.Errorf("provider = %q, want databricks-prod-oai", p.Provider)
			}
		})
	}
}

// TestPlanCheckAndBig pins the leading check token and the --1m flag, which is consumed on every path.
func TestPlanCheckAndBig(t *testing.T) {
	seedOverlay(t, allEnvTwins)
	tests := []struct {
		name      string
		args      []string
		wantCheck bool
		wantBig   bool
		wantProv  string
		wantMod   string
		wantRest  []string
	}{
		{"check alone", []string{"check"}, true, false, "databricks-qa", "databricks-claude-opus-5-5[1m]", nil},
		{"check --1m", []string{"check", "--1m"}, true, true, "databricks-qa", "databricks-claude-opus-5-5[1m]", nil},
		{"check model env --1m", []string{"check", "haiku", "--env", "dev", "--1m"}, true, true, "databricks-dev", "haiku", nil},
		{"check router model", []string{"check", "gpt-4"}, true, false, "databricks-qa-oai", "gpt-4", nil},
		{"--1m without check is consumed", []string{"sonnet", "--1m", "-p", "x"}, false, true, "databricks-qa", "sonnet", []string{"-p", "x"}},
		{"check not first is a rest arg", []string{"sonnet", "check"}, false, false, "databricks-qa", "sonnet", []string{"check"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := plan(tt.args)
			if err != nil {
				t.Fatalf("plan(%q) error: %v", tt.args, err)
			}
			if p.Check != tt.wantCheck || p.Big != tt.wantBig {
				t.Errorf("check=%v big=%v, want check=%v big=%v", p.Check, p.Big, tt.wantCheck, tt.wantBig)
			}
			if p.Provider != tt.wantProv || p.Model != tt.wantMod {
				t.Errorf("got provider %q model %q, want %q %q", p.Provider, p.Model, tt.wantProv, tt.wantMod)
			}
			if !slices.Equal(p.Rest, tt.wantRest) {
				t.Errorf("rest = %q, want %q", p.Rest, tt.wantRest)
			}
		})
	}
}

// TestPlanEnvErrors pins the env validation, including --env with no value.
func TestPlanEnvErrors(t *testing.T) {
	tests := map[string][]string{
		"--env staging":          {"--env", "staging"},
		"--env= staging":         {"--env=staging"},
		"--env with no value":    {"--env"},
		"--env= (empty)":         {"--env="},
		"--env eats a flag":      {"--env", "--1m"},
		"--env after model, bad": {"sonnet", "--env", "stage"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := plan(args)
			if err == nil || !strings.Contains(err.Error(), "must be dev, qa or prod") {
				t.Fatalf("plan(%q) error = %v, want env error", args, err)
			}
		})
	}
}

// TestRunHelpOnlyFirstOrAfterCheck pins that -h is help only as the first arg or right after check.
func TestRunHelpOnlyFirstOrAfterCheck(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, args := range [][]string{{"-h"}, {"--help"}, {"check", "-h"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := &fakeChild{}
			useFake(t, f)
			code, _, _ := runCaptured(t, func() int { return Run(args) })
			if code != 0 {
				t.Errorf("exit = %d, want 0", code)
			}
			if len(f.calls) != 0 {
				t.Errorf("help launched the child %d time(s)", len(f.calls))
			}
		})
	}
}

// TestRunLaterHelpPassesThrough pins that -h after a model reaches claude, as the shell wrapper does.
func TestRunLaterHelpPassesThrough(t *testing.T) {
	tests := [][]string{{"sonnet", "-p", "-h"}, {"--env", "dev", "-h"}}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := &fakeChild{}
			useFake(t, f)
			if code := Run(args); code != 0 {
				t.Fatalf("exit = %d, want 0", code)
			}
			if len(f.calls) != 1 || !slices.Contains(f.calls[0].argv, "-h") {
				t.Fatalf("calls = %+v, want one launch carrying -h", f.calls)
			}
		})
	}
}

// TestRunEnvErrorExit2 pins exit 2 and no child start for a bad env.
func TestRunEnvErrorExit2(t *testing.T) {
	f := &fakeChild{}
	useFake(t, f)
	code, _, stderr := runCaptured(t, func() int { return Run([]string{"--env", "staging"}) })
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "must be dev, qa or prod") {
		t.Errorf("stderr = %q, want env error", stderr)
	}
	if len(f.calls) != 0 {
		t.Errorf("bad env launched the child")
	}
}

// TestLaunchArgvNoShellAndBetaEnv pins the list-argv launch and the experimental-beta opt-out.
func TestLaunchArgvNoShellAndBetaEnv(t *testing.T) {
	seedOverlay(t, "") // a real preset row carries the beta key, which suppresses the injected opt-out
	f := &fakeChild{}
	useFake(t, f)
	if code := Run([]string{"sonnet", "--env", "prod", "-p", "hi"}); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(f.calls) != 1 {
		t.Fatalf("spawns = %d, want 1", len(f.calls))
	}
	c := f.calls[0]
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if c.bin != self {
		t.Errorf("bin = %q, want this binary %q", c.bin, self)
	}
	want := []string{"--provider", "databricks-prod", "--model", "sonnet", "-p", "hi"}
	if !slices.Equal(c.argv, want) {
		t.Errorf("argv = %q, want %q", c.argv, want)
	}
	for _, shell := range []string{"sh", "bash", "-c"} {
		if slices.Contains(c.argv, shell) || strings.HasSuffix(c.bin, shell) {
			t.Errorf("launch looks shell-wrapped via %q", shell)
		}
	}
	if !slices.Contains(c.env, betaOff) {
		t.Errorf("env lacks %s", betaOff)
	}
}

// TestLaunchPropagatesExitAndStartError pins the child's exit code and exit 2 on a start failure.
func TestLaunchPropagatesExitAndStartError(t *testing.T) {
	t.Run("child exit 7", func(t *testing.T) {
		useFake(t, &fakeChild{code: 7})
		if code := Run([]string{"sonnet"}); code != 7 {
			t.Errorf("exit = %d, want 7", code)
		}
	})
	t.Run("start failure", func(t *testing.T) {
		useFake(t, &fakeChild{err: os.ErrNotExist})
		code, _, stderr := runCaptured(t, func() int { return Run([]string{"sonnet"}) })
		if code != 2 {
			t.Errorf("exit = %d, want 2", code)
		}
		if !strings.Contains(stderr, "exec failed") {
			t.Errorf("stderr = %q, want exec failed", stderr)
		}
	})
}

// TestCheckSmokeVerdict pins that the smoke PASS needs the last line to be exactly OK.
func TestCheckSmokeVerdict(t *testing.T) {
	tests := []struct {
		name   string
		out    string
		wantOK bool
	}{
		{"bare OK", "OK\n", true},
		{"OK no newline", "OK", true},
		{"OK after noise", "thinking...\nOK\n", true},
		{"OK then more text", "OK\nmore\n", false},
		{"substring only", "NOT OK\n", false},
		{"empty output", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useFake(t, &fakeChild{smoke: tt.out})
			code, stdout, _ := runCaptured(t, func() int { return Run([]string{"check"}) })
			if tt.wantOK {
				if code != 0 || !strings.Contains(stdout, "PASS smoke") {
					t.Errorf("exit %d, stdout %q; want PASS smoke and exit 0", code, stdout)
				}
				return
			}
			if code != 1 || !strings.Contains(stdout, "FAIL smoke") {
				t.Errorf("exit %d, stdout %q; want FAIL smoke and exit 1", code, stdout)
			}
		})
	}
}

// TestCheckNeedleVerdict pins that the 1M PASS needs the needle word anywhere in the reply.
func TestCheckNeedleVerdict(t *testing.T) {
	tests := []struct {
		name   string
		out    string
		wantOK bool
	}{
		{"word only", "ZEBRAFINCH\n", true},
		{"word in sentence", "The word is ZEBRAFINCH.", true},
		{"no word", "I don't know\n", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useFake(t, &fakeChild{smoke: "OK\n", needle: tt.out})
			code, stdout, _ := runCaptured(t, func() int { return Run([]string{"check", "--1m"}) })
			if tt.wantOK {
				if code != 0 || !strings.Contains(stdout, "1m PASS") {
					t.Errorf("exit %d, stdout %q; want 1m PASS and exit 0", code, stdout)
				}
				return
			}
			if code != 1 || !strings.Contains(stdout, "FAIL 1m") {
				t.Errorf("exit %d, stdout %q; want FAIL 1m and exit 1", code, stdout)
			}
		})
	}
}

// TestCheckFailingSmokeStillRunsProbe pins that a smoke FAIL does not skip the 1M probe, and rc stays 1.
func TestCheckFailingSmokeStillRunsProbe(t *testing.T) {
	f := &fakeChild{smoke: "nope\n", needle: "ZEBRAFINCH\n"}
	useFake(t, f)
	code, stdout, _ := runCaptured(t, func() int { return Run([]string{"check", "--1m"}) })
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stdout, "FAIL smoke") || !strings.Contains(stdout, "1m PASS") {
		t.Errorf("stdout = %q, want FAIL smoke then PASS 1m", stdout)
	}
	if len(f.calls) != 2 {
		t.Errorf("spawns = %d, want 2", len(f.calls))
	}
}

// TestCheckWithoutBigSkipsProbe pins that plain check makes one call and no needle body.
func TestCheckWithoutBigSkipsProbe(t *testing.T) {
	f := &fakeChild{smoke: "OK\n"}
	useFake(t, f)
	code, stdout, _ := runCaptured(t, func() int { return Run([]string{"check"}) })
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if len(f.calls) != 1 || f.body != nil {
		t.Errorf("spawns = %d, probe body len = %d; want 1 spawn and no probe", len(f.calls), len(f.body))
	}
	if strings.Contains(stdout, "== 1m") {
		t.Errorf("stdout mentions 1m without --1m: %q", stdout)
	}
}

// TestNeedleProbeShape pins the 1M probe body: at least fillerBytes, needle line exactly once, at the end.
func TestNeedleProbeShape(t *testing.T) {
	f := &fakeChild{smoke: "OK\n", needle: "ZEBRAFINCH\n"}
	useFake(t, f)
	runCaptured(t, func() int { return Run([]string{"check", "--1m"}) })
	body := string(f.body)
	if len(body) < fillerBytes {
		t.Errorf("probe body %d bytes, want >= %d", len(body), fillerBytes)
	}
	if !strings.HasSuffix(body, needleText) {
		t.Errorf("probe body does not end with the needle line")
	}
	if n := strings.Count(body, needleWord); n != 1 {
		t.Errorf("needle appears %d times, want 1", n)
	}
}

func TestIsClaudeModel(t *testing.T) {
	for m, want := range map[string]bool{
		"opus": true, "databricks-claude-opus-5-5": true, "claude-opus-5-5": true, "claude-opus-5-5[1m]": true,
		"gpt-5.5": false, "databricks-gpt-5-5": false, "my-opus": false, "": false,
	} {
		if got := isClaudeModel(m); got != want {
			t.Errorf("isClaudeModel(%q) = %v, want %v", m, got, want)
		}
	}
}

// TestParseUsage pins the usage parse: cache fields are summed, and non-JSON or empty output yields zero tokens.
func TestParseUsage(t *testing.T) {
	tests := []struct {
		name       string
		out        string
		wantText   string
		wantTokens int
	}{
		{"sums cache fields", `{"result":"ZEBRAFINCH","usage":{"input_tokens":1,"cache_creation_input_tokens":20,"cache_read_input_tokens":300}}`, "ZEBRAFINCH", 321},
		{"last JSON line wins", "noise\n{\"result\":\"a\",\"usage\":{\"input_tokens\":5}}\n", "a", 5},
		{"non-JSON", "ZEBRAFINCH\n", "ZEBRAFINCH\n", 0},
		{"empty", "", "", 0},
		{"no usage", `{"result":"x"}`, "x", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, tokens := parseUsage(tt.out)
			if text != tt.wantText || tokens != tt.wantTokens {
				t.Errorf("parseUsage = (%q, %d), want (%q, %d)", text, tokens, tt.wantText, tt.wantTokens)
			}
		})
	}
}

// rawNeedleChild answers the needle prompt with a verbatim stdout, so tests control the usage block.
type rawNeedleChild struct{ out string }

func (f rawNeedleChild) spawn(_ string, argv, _ []string, stdin io.Reader, stdout, _ io.Writer) (int, error) {
	if argValue(argv, "-p") == needlePrompt {
		_, _ = io.Copy(io.Discard, stdin)
		_, _ = io.WriteString(stdout, f.out)
		return 0, nil
	}
	_, _ = io.WriteString(stdout, "OK\n")
	return 0, nil
}

// TestCheckNeedleTokenGate pins that PASS needs input tokens strictly above 200000, plus the needle in the reply.
func TestCheckNeedleTokenGate(t *testing.T) {
	js := func(reply string, in, create, read int) string {
		b, _ := json.Marshal(map[string]any{"result": reply, "usage": map[string]int{
			"input_tokens": in, "cache_creation_input_tokens": create, "cache_read_input_tokens": read}})
		return string(b) + "\n"
	}
	tests := []struct {
		name     string
		out      string
		wantPass bool
		wantMsg  string
	}{
		{"above", js("ZEBRAFINCH", 200001, 0, 0), true, "1m PASS"},
		{"exactly 200000", js("ZEBRAFINCH", 200000, 0, 0), false, "input_tokens=200000"},
		{"below", js("ZEBRAFINCH", 1000, 0, 0), false, "input_tokens=1000"},
		{"cache summed over", js("ZEBRAFINCH", 1, 150000, 60000), true, "input_tokens=210001"},
		{"cache summed at limit", js("ZEBRAFINCH", 0, 100000, 100000), false, "input_tokens=200000"},
		{"needle missing, tokens ok", js("no idea", 300000, 0, 0), false, "needle not found"},
		{"plain text word, no usage", "ZEBRAFINCH\n", false, "input_tokens=0"},
		{"garbage", "{not json", false, "FAIL 1m"},
		{"empty", "", false, "needle not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := spawn
			spawn = rawNeedleChild{tt.out}.spawn
			t.Cleanup(func() { spawn = old })
			code, stdout, _ := runCaptured(t, func() int { return Run([]string{"check", "--1m"}) })
			if !strings.Contains(stdout, tt.wantMsg) {
				t.Errorf("stdout %q missing %q", stdout, tt.wantMsg)
			}
			if tt.wantPass != (code == 0) || tt.wantPass == strings.Contains(stdout, "FAIL 1m") {
				t.Errorf("exit %d, stdout %q; wantPass=%v", code, stdout, tt.wantPass)
			}
		})
	}
}
