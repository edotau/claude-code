package ask

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/pack"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hooks"
	"github.com/edotau/claude-code/internal/router"
)

// Given pack flags that cannot work together or alone, askPack refuses each; valid ones build the request's spec.
func TestAskPackFlagCombos(t *testing.T) {
	refused := []struct {
		name string
		args []string
		want string
	}{
		{"index alone", []string{"--index"}, "--index needs"},
		{"max-files alone", []string{"--max-files", "3"}, "--max-files needs"},
		{"max-file-bytes alone", []string{"--max-file-bytes", "9"}, "--max-file-bytes needs"},
		{"negative max-files", []string{"--dirs", ".", "--max-files", "-1"}, "negative"},
		{"batch", []string{"--dirs", ".", "--batch", "-"}, "--batch"},
		{"live", []string{"--files", "*.go", "--live"}, "--live"},
		{"remote", []string{"--diff", "--host", "box"}, "--host"},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			fs, f := askFlagSet()
			if err := fs.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			if _, err := askPack(fs, f); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("askPack(%v) = %v, want an error containing %q", tt.args, err, tt.want)
			}
		})
	}
	fs, f := askFlagSet()
	if err := fs.Parse([]string{"--dirs", "a,b", "--index", "--max-files", "0", "--diff"}); err != nil {
		t.Fatal(err)
	}
	f.pack, _ = askPack(fs, f)
	req := (&askRun{ctx: t.Context(), f: f}).request("task")
	if p := req.Pack; p == nil || len(p.Dirs) != 2 || !p.Diff || !p.Index || !p.IndexOnly {
		t.Fatalf("request.Pack = %+v, want dirs a,b + diff + index + index-only", p)
	}
}

// Ties the --effort help to the harness scale.
func TestEffortFlagHelpListsTheScale(t *testing.T) {
	fs, _ := askFlagSet()
	if got := fs.Lookup("effort").Usage; got != "reasoning effort: low|medium|high|xhigh|max" {
		t.Errorf("--effort usage = %q", got)
	}
}

// attachFlag.Set resolves a real file to an absolute path and errors on anything else.
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

// --keep-env comma-splits each occurrence and accumulates across repeats.
func TestKeepEnvFlagSet(t *testing.T) {
	fs, f := askFlagSet()
	if err := fs.Parse([]string{"--keep-env", "DATABRICKS_HOST,DATABRICKS_TOKEN", "--keep-env", "HOME"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !slices.Equal(f.keepEnv, []string{"DATABRICKS_HOST", "DATABRICKS_TOKEN", "HOME"}) {
		t.Errorf("keepEnv = %q, want accumulated comma-split names", f.keepEnv)
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

func newAskTarget() *askTarget {
	s := func() *string { return new(string) }
	b := func() *bool { return new(bool) }
	return &askTarget{Name: s(), Provider: s(), Model: s(), Effort: s(), Workdir: s(), Session: s(),
		Tools: b(), ReadOnly: b(), NoTools: b(), Autonomous: b(), Fleet: b(), EnableMemory: b(), Direct: b(),
		NoPersist: b(), SafeMode: b(), DebugFile: s(), SystemPrompt: s(), AppendSystemPrompt: s(), OutputSchema: s(),
		Host: s(), Fallback: s(), Timeout: new(time.Duration), Attach: new([]string), KeepEnv: new([]string),
		PermMode: s(), PermAllow: new([]string), PermAsk: new([]string), PermDeny: new([]string),
		Native: b(), Pack: new(*pack.Spec), Persona: new(*agent.Schema)}
}

// A retried or backgrounded dispatch must keep --no-tools; without it the tools come back on.
func TestApplyRecordRoundTripsNoTools(t *testing.T) {
	s := newAskTarget()
	s.Apply(agent.Record{Agent: "claude", Request: agent.RunOptions{NoTools: true}}, io.Discard)
	if !*s.NoTools || *s.Tools {
		t.Errorf("noTools=%v tools=%v; want noTools on, tools off", *s.NoTools, *s.Tools)
	}
	// Negative control: a record without NoTools clears an earlier value.
	*s.NoTools = true
	s.Apply(agent.Record{Agent: "claude", Request: agent.RunOptions{Tools: true}}, io.Discard)
	if *s.NoTools || !*s.Tools {
		t.Errorf("noTools=%v tools=%v; want tools on, noTools off", *s.NoTools, *s.Tools)
	}
}

// A replayed read-only dispatch must stay read-only; the flag default would hand it write tools.
func TestApplyRecordRoundTripsReadOnly(t *testing.T) {
	s := newAskTarget()
	s.Apply(agent.Record{Agent: "claude", Request: agent.RunOptions{ReadOnly: true, Tools: true}}, io.Discard)
	if !*s.ReadOnly || !*s.Tools {
		t.Errorf("readOnly=%v tools=%v; want both on", *s.ReadOnly, *s.Tools)
	}
	s.Apply(agent.Record{Agent: "claude", Request: agent.RunOptions{Tools: true}}, io.Discard)
	if *s.ReadOnly {
		t.Error("readOnly stayed on for a record without it")
	}
}

func TestResolveAskPromptBoundsStdin(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdin
	os.Stdin = r
	writerDone := make(chan struct{})
	go writeOversizePrompt(w, writerDone)
	t.Cleanup(func() {
		os.Stdin = original
		_ = r.Close()
		_ = w.Close()
		<-writerDone
	})
	type result struct {
		prompt string
		err    error
	}
	results := make(chan result, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		prompt, err := resolveAskPrompt("-")
		results <- result{prompt, err}
	}()
	t.Cleanup(func() { _ = r.Close(); <-readerDone })
	select {
	case got := <-results:
		want := agent.PromptTooLarge(agent.MaxPromptBytes+1, agent.MaxPromptBytes)
		if got.prompt != "" || !errors.Is(got.err, agent.ErrPromptTooLarge) || got.err.Error() != want.Error() {
			t.Fatalf("resolveAskPrompt = (%q, %v), want empty prompt and %q", got.prompt, got.err, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resolveAskPrompt waited for EOF instead of stopping at MaxPromptBytes+1")
	}
}

func writeOversizePrompt(w *os.File, done chan<- struct{}) {
	defer close(done)
	chunk := []byte(strings.Repeat("x", 4<<10))
	for range 4096 {
		if _, err := w.Write(chunk); err != nil {
			return
		}
	}
	// Keep the write end open until cleanup: even a finite oversized stream must not require EOF.
}

func TestAskJobRestoresRecordedLead(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.NewRecord("claude", agent.RunOptions{Prompt: "say ok", Lead: "qa"}, time.Now())
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	fs, f := askFlagSet()
	if err := fs.Parse([]string{"--job", rec.ID}); err != nil {
		t.Fatal(err)
	}
	r := &askRun{ctx: t.Context(), fs: fs, f: f}
	if _, code, done := loadAskJob(f.job, f.format, r.askState(), &r.prompt); done || code != 0 {
		t.Fatalf("loadAskJob = %d, %v", code, done)
	}
	if got := r.request(r.prompt); got.Lead != "qa" {
		t.Fatalf("job lead = %q, want qa", got.Lead)
	}
}

func TestAskRetryPreservesRecordedLead(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.NewRecord("claude", agent.RunOptions{Prompt: "say ok", Lead: "recorded-profile"}, time.Now())
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, flag, wantLead, wantRoute string
	}{
		{"recorded lead", "", "recorded-profile", ""},
		{"profile override", "--profile", "", "replacement-profile"},
		{"provider override", "--provider", "", "replacement-profile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"--retry", rec.ID}
			if tc.flag != "" {
				args = append(args, tc.flag, "replacement-profile")
			}
			fs, f := askFlagSet()
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			r := &askRun{ctx: t.Context(), fs: fs, f: f}
			if code, done := loadAskRetry(t.Context(), fs, args, f.retry, f.format, r.askState(), &r.prompt); done || code != 0 {
				t.Fatalf("loadAskRetry = %d, %v", code, done)
			}
			if got := r.request(r.prompt); got.Lead != tc.wantLead {
				t.Fatalf("replayed lead = %q, want %q", got.Lead, tc.wantLead)
			}
			if got := f.provider + f.leadProvider; got != tc.wantRoute {
				t.Fatalf("explicit route = %q, want %q", got, tc.wantRoute)
			}
		})
	}
}

// --retry keeps the recorded fallback policy over AGENT_ERROR_FALLBACK; a record without one takes the env mode.
func TestAskRetryFallbackFromEnv(t *testing.T) {
	for _, tc := range []struct{ recorded, env, want string }{
		{"pin", "all", "pin"}, {"none", "1", "none"}, {"", "pin", "pin"}, {"", "none", "none"}, {"", "1", "all"}, {"", "", "none"},
	} {
		t.Run(tc.recorded+"/"+tc.env, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			t.Setenv(router.EnvFallback, tc.env)
			rec := agent.NewRecord("claude", agent.RunOptions{Prompt: "p"}, time.Now())
			rec.FallbackPolicy = tc.recorded
			if err := rec.Save(); err != nil {
				t.Fatal(err)
			}
			args := []string{"--retry", rec.ID}
			fs, f := askFlagSet()
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			f.fallback = router.DefaultFallback() // runTo does this before the phases when --fallback is not typed
			r := &askRun{ctx: t.Context(), fs: fs, f: f}
			if code, done := loadAskRetry(t.Context(), fs, args, f.retry, f.format, r.askState(), &r.prompt); done || code != 0 {
				t.Fatalf("loadAskRetry = %d, %v", code, done)
			}
			if f.fallback != tc.want {
				t.Fatalf("fallback = %q, want %q", f.fallback, tc.want)
			}
		})
	}
}

// Given a limit-only, budget, env or codexNetwork overlay, overlayLocal refuses --live/--host for all but limits.
func TestOverlayLocalRefusalNamesKeys(t *testing.T) {
	cases := []struct {
		name, doc, want string
	}{
		{"limits pass", `{"maxChildren":2,"maxTreeTokens":9}`, ""},
		{"budget", `{"budgetUSD":1.5}`, "budgetUSD/budgetCredits"},
		{"env", `{"set":{"A":"b"}}`, "set/unset/keep cannot"},
		{"codexNetwork", `{"codexNetwork":true}`, "overlay codexNetwork cannot"},
	}
	for _, c := range cases {
		ov, err := agentconfig.LoadOverlayContext(context.Background(), c.doc, nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		err = overlayLocal(ov, true, "")
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err %v, want %q", c.name, err, c.want)
		}
	}
}

// Flags parse in any position and the dispatch hooks predict the same route; "--" ends flags, an unknown one exits 2.
func TestParseAskArgsFlagPositions(t *testing.T) {
	rows := []struct {
		name    string
		args    []string
		pos     []string
		tools   bool
		profile string
		code    int
	}{
		{"flag after prompt", []string{"do x", "--tools"}, []string{"do x"}, true, "", 0},
		{"flag before prompt", []string{"--tools", "do x"}, []string{"do x"}, true, "", 0},
		{"value flag after prompt", []string{"do x", "--profile", "qa"}, []string{"do x"}, false, "qa", 0},
		{"value flag before prompt", []string{"--profile", "qa", "do x"}, []string{"do x"}, false, "qa", 0},
		{"flag=value after prompt", []string{"do x", "--profile=qa"}, []string{"do x"}, false, "qa", 0},
		{"flag=value before prompt", []string{"--profile=qa", "do x"}, []string{"do x"}, false, "qa", 0},
		{"after -- is verbatim", []string{"--tools", "--", "a", "--tools", "--profile", "qa"}, []string{"a", "--tools", "--profile", "qa"}, true, "", 0},
		{"dash prompt after --", []string{"--", "-1 is wrong"}, []string{"-1 is wrong"}, false, "", 0},
		{"dash prompt without --", []string{"-1 is wrong"}, nil, false, "", 2},
		{"unknown trailing flag", []string{"do x", "--foo"}, nil, false, "", 2},
		{"quoted prompt with a flag word", []string{"explain --tools please"}, []string{"explain --tools please"}, false, "", 0},
		{"stdin dash", []string{"-", "--tools"}, []string{"-"}, true, "", 0},
	}
	for _, r := range rows {
		fs, f := askFlagSet()
		fs.SetOutput(io.Discard)
		pos, code, _ := parseAskArgs(fs, r.args)
		if code != r.code || !slices.Equal(pos, r.pos) || f.tools != r.tools || f.provider != r.profile {
			t.Errorf("%s: got pos %q code %d tools %v profile %q, want %q %d %v %q", r.name, pos, code, f.tools, f.provider, r.pos, r.code, r.tools, r.profile)
		}
		w, ok := hooks.ParseAskWords(r.args)
		if ok != (r.code == 0) || ok && (!slices.Equal(w.Pos, r.pos) || w.Has("tools") != r.tools || w.Value("profile") != r.profile) {
			t.Errorf("%s: hook predicts pos %q ok %v tools %v profile %q, ask ran %q %d %v %q",
				r.name, w.Pos, ok, w.Has("tools"), w.Value("profile"), r.pos, r.code, r.tools, r.profile)
		}
	}
}

// Every flag ask registers parses the same in the hooks' grammar: a bool takes no value, any other takes the next word.
func TestHookGrammarCoversEveryAskFlag(t *testing.T) {
	fs, _ := askFlagSet()
	fs.VisitAll(func(f *flag.Flag) {
		args := []string{"--" + f.Name, "v", "task"}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			args = []string{"--" + f.Name, "task"}
		}
		w, ok := hooks.ParseAskWords(args)
		if !ok || !w.Has(f.Name) || !slices.Equal(w.Pos, []string{"task"}) {
			t.Errorf("--%s: hooks parse %q ok %v pos %q", f.Name, args, ok, w.Pos)
		}
	})
}

func TestRefuseAgentWord(t *testing.T) {
	for _, tc := range []struct {
		args []string
		bad  bool
	}{
		{[]string{"copilot", "reply OK"}, true},
		{[]string{"--agent", "copilot", "copilot", "x"}, false},
		{[]string{"--", "copilot", "is", "great"}, false},
		{[]string{"codexes", "go"}, false},
	} {
		fs, _ := askFlagSet()
		pos, code, done := parseAskArgs(fs, tc.args)
		if done {
			t.Fatalf("%v: parse exit %d", tc.args, code)
		}
		if err := refuseAgentWord(fs, "", tc.args, pos); (err != nil) != tc.bad {
			t.Errorf("%v: err=%v, want refusal=%v", tc.args, err, tc.bad)
		}
	}
}
