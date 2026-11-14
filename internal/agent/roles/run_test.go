package roles

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	_ "github.com/edotau/claude-code/internal/codex" // the vendors register their schemas from their own init
	_ "github.com/edotau/claude-code/internal/copilot"
	_ "github.com/edotau/claude-code/internal/gemini" // registers the gemini schema (ReadNeedsTools)
	"github.com/edotau/claude-code/internal/hosttest"
	_ "github.com/edotau/claude-code/internal/opencode"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// fakeStream swaps streamLeg for a stub keyed by model; models in failOn return a failed envelope.
func fakeStream(t *testing.T, failOn ...string) *[]agent.RunOptions {
	t.Helper()
	var mu sync.Mutex
	var seen []agent.RunOptions
	orig := streamLeg
	streamLeg = func(_ *agent.Client, _ context.Context, o agent.RunOptions) <-chan wire.Event {
		mu.Lock()
		seen = append(seen, o)
		mu.Unlock()
		env := wire.Envelope{Status: "ok", Agent: o.Agent, Model: o.Model, Answer: "answer " + o.Model}
		for _, m := range failOn {
			if m == o.Model {
				env = wire.FailureEnvelope(o.Agent, errors.New("boom"))
			}
		}
		ch := make(chan wire.Event, 1)
		ch <- wire.ResultEvent(env)
		close(ch)
		return ch
	}
	t.Cleanup(func() { streamLeg = orig })
	return &seen
}

func TestRoleExploreLegsOnePerPart(t *testing.T) {
	vaulttest.Use(t)
	r := Role{Name: "explore", Agent: "copilot"}
	legs := Legs(r, Request{Task: "t", Parts: []string{"a", "b"}, WorkDir: "/w", Web: true})
	if len(legs) != 2 || legs[0].Label != "a" || legs[1].Label != "b" {
		t.Fatalf("want legs a,b; got %+v", legs)
	}
	o := legs[1].Opts
	if o.Agent != "copilot" || !o.Tools || !o.ReadOnly || o.WorkDir != "/w" || o.Prompt != prompt("b", true) {
		t.Errorf("explore opts wrong: %+v", o)
	}
}

func TestRoleLegsCarryOutputSchema(t *testing.T) {
	vaulttest.Use(t)
	const schema = `{"type":"object"}`
	for _, tc := range []struct{ role, models string }{{"explore", ""}, {"fleet", ""}, {"compare", ""}} {
		r := mustRole(t, tc.role)
		if r.Name == "explore" {
			r.Agent = "copilot"
		}
		legs := Legs(r, Request{Task: "t", Parts: []string{"a"}, WorkDir: "/w", OutputSchema: schema})
		if len(legs) == 0 {
			t.Fatalf("%s: no legs", tc.role)
		}
		for _, l := range legs {
			if l.Err == "" && l.Opts.OutputSchema != schema {
				t.Errorf("%s leg %q: OutputSchema = %q, want %q", tc.role, l.Label, l.Opts.OutputSchema, schema)
			}
		}
	}
}

func TestRoleExploreNoPartsUsesTask(t *testing.T) {
	vaulttest.Use(t)
	legs := Legs(Role{Name: "explore", Agent: "codex"}, Request{Task: "whole", Web: false})
	if len(legs) != 1 || legs[0].Opts.Prompt != prompt("whole", false) {
		t.Fatalf("want one leg on the task; got %+v", legs)
	}
}

// An auto-picked workspace marks every leg's options, so a detached leg's job child can still move on a 429.
func TestLegsCarryAutoArm(t *testing.T) {
	vaulttest.Use(t)
	for auto, want := range map[string]bool{"dev": true, "": false} {
		for _, l := range Legs(mustRole(t, "fleet"), Request{Task: "t", Auto: auto}) {
			if l.Opts.AutoArm != want {
				t.Errorf("Auto %q: leg %s AutoArm = %v, want %v", auto, l.Label, l.Opts.AutoArm, want)
			}
		}
	}
}

func TestRoleFleetOneLeg(t *testing.T) {
	vaulttest.Use(t)
	legs := Legs(mustRole(t, "fleet"), Request{Task: "do it"})
	if len(legs) != 1 || legs[0].Label != "fleet" {
		t.Fatalf("want one fleet leg; got %+v", legs)
	}
	if o := legs[0].Opts; o.Agent != "copilot" || !o.Fleet || !o.Tools || o.Prompt != "do it" {
		t.Errorf("fleet opts wrong: %+v", o)
	}
}

func TestRoleCompareUnknownModelPreFailed(t *testing.T) {
	vaulttest.Use(t)
	seen := fakeStream(t)
	legs := Legs(mustRole(t, "compare"), Request{Task: "q", Models: []string{"claude-haiku-5-5", "bogus"}})
	if legs[0].Err != "" || legs[1].Err != "not on the opencode menu" {
		t.Fatalf("errs: %q %q", legs[0].Err, legs[1].Err)
	}
	rep := Run(context.Background(), nil, mustRole(t, "compare"), legs, 4, nil)
	var out bytes.Buffer
	rep.Text(&out)
	if rep.Status != "partial" || len(*seen) != 1 || (*seen)[0].Model != "claude-haiku-5-5" {
		t.Errorf("status=%s streamed=%v", rep.Status, *seen)
	}
	if !strings.HasSuffix(out.String(), "failed: bogus\n") || !strings.Contains(out.String(), "not on the opencode menu") {
		t.Errorf("text:\n%s", out.String())
	}
}

type failingReportWriter struct{}

func (failingReportWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestReportTextPropagatesWriteError(t *testing.T) {
	for _, size := range []int{16, 8192} {
		rep := Report{Status: "ok", Legs: []LegResult{{
			Label:    "successful dispatch",
			Envelope: wire.Envelope{Status: "ok", Agent: "codex", Answer: strings.Repeat("x", size)},
		}}}
		if err := rep.Text(failingReportWriter{}); !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("answer size %d: lost report write error: got %v, want %v", size, err, io.ErrClosedPipe)
		}
	}
}

func TestRoleCompareDefaultsToRoleModels(t *testing.T) {
	vaulttest.Use(t)
	r := Role{Name: "compare", Agent: "opencode", Models: []string{"kimi-k3"}}
	legs := Legs(r, Request{Task: "q"})
	if len(legs) != 1 || legs[0].Opts.Model != "kimi-k3" || legs[0].Opts.Prompt != "q" {
		t.Fatalf("got %+v", legs)
	}
}

func TestRunStatusOkPartialError(t *testing.T) {
	vaulttest.Use(t)
	r := mustRole(t, "compare")
	mk := func(ms ...string) []Leg {
		var legs []Leg
		for _, m := range ms {
			legs = append(legs, Leg{Label: m, Opts: agent.RunOptions{Agent: "opencode", Model: m}})
		}
		return legs
	}
	for _, tc := range []struct {
		fail []string
		want string
	}{{nil, "ok"}, {[]string{"b"}, "partial"}, {[]string{"a", "b"}, "error"}} {
		fakeStream(t, tc.fail...)
		rep := Run(context.Background(), nil, r, mk("a", "b"), 2, nil)
		if rep.Status != tc.want {
			t.Errorf("fail=%v: status %s, want %s", tc.fail, rep.Status, tc.want)
		}
		if tc.want == "partial" && (rep.Legs[0].Envelope.Answer != "answer a" || rep.Legs[1].Envelope.Status == "ok") {
			t.Errorf("partial lost the surviving answer: %+v", rep.Legs)
		}
	}
}

func TestRunProgressTagsWholeLines(t *testing.T) {
	orig := streamLeg
	streamLeg = func(_ *agent.Client, _ context.Context, o agent.RunOptions) <-chan wire.Event {
		ch := make(chan wire.Event, 4)
		for _, s := range []string{"hel", "lo\nwor", "ld"} {
			ch <- wire.Event{Type: wire.EventText, Text: s}
		}
		ch <- wire.ResultEvent(wire.Envelope{Status: "ok"})
		close(ch)
		return ch
	}
	t.Cleanup(func() { streamLeg = orig })
	var out bytes.Buffer
	Run(context.Background(), nil, Role{Name: "fleet"}, []Leg{{Label: "x"}}, 1, &out)
	if got := out.String(); got != "[x] hello\n[x] world\n" {
		t.Errorf("progress = %q", got)
	}
}

// movedOrSilent is a closed stream: one ok result that moved dev → qa, or no event at all.
func movedOrSilent(moved bool) <-chan wire.Event {
	ch := make(chan wire.Event, 1)
	if moved {
		ch <- wire.ResultEvent(wire.Envelope{Status: "ok", Answer: "from the runner", Hops: []wire.Hop{{Profile: "dev"}, {Profile: "qa"}}})
	}
	close(ch)
	return ch
}

// RunWith keeps the injected runner's envelope; a pre-failed leg never reaches it, a silent runner fails its leg.
func TestRunWithInjectedRunner(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	runner := func(_ context.Context, o agent.RunOptions) <-chan wire.Event {
		mu.Lock()
		ran = append(ran, o.Prompt)
		mu.Unlock()
		return movedOrSilent(o.Prompt == "moved")
	}
	legs := []Leg{{Label: "moved", Opts: agent.RunOptions{Prompt: "moved"}}, {Label: "silent", Opts: agent.RunOptions{Prompt: "silent"}},
		{Label: "refused", Opts: agent.RunOptions{Prompt: "refused"}, Err: "not allowed"}}
	rep := RunWith(context.Background(), Role{Name: "fleet"}, legs, 3, nil, runner)
	msg := func(l LegResult) string {
		if l.Envelope.Error == nil {
			return ""
		}
		return l.Envelope.Error.Message
	}
	got := rep.Legs
	if rep.Status != "partial" || got[0].Envelope.Answer != "from the runner" || len(got[0].Envelope.Hops) != 2 {
		t.Errorf("status %q legs %+v, want partial with the runner's envelope and both hops kept", rep.Status, got)
	}
	if !strings.Contains(msg(got[1]), "no result event") || !strings.Contains(msg(got[2]), "not allowed") {
		t.Errorf("silent leg %q, refused leg %q, want the missing-result and the pre-failure errors", msg(got[1]), msg(got[2]))
	}
	if slices.Contains(ran, "refused") {
		t.Errorf("runner saw %v, want the refused leg kept from it", ran)
	}
}

// A role's permissions ride every leg as the role layer ResolvePermissions puts over the agent's defaults.
func TestLegsCarryRolePermissions(t *testing.T) {
	p, err := agent.ParsePermissions("", nil, nil, []string{"Bash(rm:*)"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"explore", "fleet", "compare"} {
		legs := Legs(Role{Name: name, Agent: "codex", Models: []string{"m"}, Permissions: p}, Request{Task: "t"})
		if len(legs) == 0 || len(legs[0].Opts.Permissions.Deny) != 1 {
			t.Errorf("%s legs %+v lost the role's deny", name, legs)
		}
	}
}

func TestLegsAddFlagPermissionsAndFallbackToRole(t *testing.T) {
	role, err := agent.ParsePermissions("plan", nil, nil, []string{"Bash(rm:*)"})
	if err != nil {
		t.Fatal(err)
	}
	flags, err := agent.ParsePermissions("auto", []string{"Bash(ls:*)"}, nil, []string{"Bash(rm:*)"})
	if err != nil {
		t.Fatal(err)
	}
	legs := Legs(Role{Name: "fleet", Agent: "codex", Permissions: role}, Request{Task: "t", Permissions: flags, Fallback: "none"})
	got := legs[0].Opts
	if got.Permissions.Mode != "auto" || len(got.Permissions.Deny) != 1 || len(got.Permissions.Allow) != 1 || got.Fallback != "none" {
		t.Errorf("leg opts %+v: want flag mode, one deduped deny, the added allow and fallback none", got)
	}
}

// --fleet is not a permission feature: a fleet role on an agent without it still runs the task and reports the drop.
func TestLegsFleetOnUnsupportedAgentRuns(t *testing.T) {
	vaulttest.Use(t)
	seen := fakeStream(t)
	r := Role{Name: "fleet", Agent: "codex"}
	legs := Legs(r, Request{Task: "t"})
	if len(legs) != 1 || legs[0].Err != "" {
		t.Fatalf("want one runnable leg; got %+v", legs)
	}
	rep := Run(context.Background(), nil, r, legs, 1, nil)
	if len(*seen) != 1 || rep.Status != "ok" {
		t.Fatalf("leg not dispatched: calls=%v report=%+v", *seen, rep)
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	hosttest.Cleanup()
	os.Exit(code)
}

func mustRole(t *testing.T, name string) Role {
	t.Helper()
	r, err := Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRoleVaultOverlayMovesExplore(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, rolesKey, `[{"name":"explore","agent":"opencode"}]`)
	r := mustRole(t, "explore")
	if r.Agent != "opencode" {
		t.Fatalf("overlay ignored: %+v", r)
	}
	if _, err := Lookup("fleet"); err == nil {
		t.Error("overlay replaces the table: fleet must be unknown")
	}
	if got := Legs(r, Request{Task: "x"})[0].Err; !strings.Contains(got, "explore on opencode") {
		t.Errorf("prompt-only overlay must be refused: %q", got)
	}
}

func assertContainsAll(t *testing.T, got string, substrs ...string) {
	t.Helper()
	for _, s := range substrs {
		if !strings.Contains(got, s) {
			t.Errorf("prompt missing %q:\n%s", s, got)
		}
	}
}

// Prompt must name all five required sections, cite file:line, and carry the question verbatim.
func TestPromptHasFiveSections(t *testing.T) {
	got := prompt("how does X work?", false)
	assertContainsAll(t, got, "Structure", "Conventions", "Dependencies", "Prior art", "Open questions",
		"file:line", "how does X work?")
}

// web=false must omit the web-research dispatch entirely (no URL-citation instruction).
func TestPromptOmitsWebWhenDisabled(t *testing.T) {
	got := prompt("q", false)
	if strings.Contains(got, "URL") || strings.Contains(got, "web") {
		t.Errorf("web=false prompt must not mention web research:\n%s", got)
	}
}

// web=true appends the web-research dispatch, which cites claims by URL.
func TestPromptIncludesWebWhenEnabled(t *testing.T) {
	got := prompt("q", true)
	assertContainsAll(t, got, "web", "URL")
}

// Args includes --tools exactly for vendors whose ReadNeedsTools is true.
func TestArgsToolsFlagPerVendor(t *testing.T) {
	cases := []struct {
		vendor    string
		wantTools bool
	}{
		{"codex", false},
		{"copilot", true},
	}
	for _, c := range cases {
		args, err := Args(c.vendor, "/work", "q", false, false)
		if err != nil {
			t.Fatal(err)
		}
		got := argvContains(args, "--tools")
		if got != c.wantTools {
			t.Errorf("Args(%q) --tools present = %v, want %v (args=%v)", c.vendor, got, c.wantTools, args)
		}
	}
}

// Args includes --background exactly when requested, independent of the vendor.
func TestArgsBackgroundFlag(t *testing.T) {
	fg, err := Args("codex", "/work", "q", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if argvContains(fg, "--background") {
		t.Errorf("foreground Args must not include --background: %v", fg)
	}
	bg, err := Args("codex", "/work", "q", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !argvContains(bg, "--background") {
		t.Errorf("background Args must include --background: %v", bg)
	}
}

// Args always carries --agent/--workdir and ends with the built prompt as the last argv element.
func TestArgsShape(t *testing.T) {
	args, err := Args("copilot", "/work", "q", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) < 3 || args[0] != "--agent" || args[1] != "copilot" {
		t.Fatalf("Args must start with --agent copilot: %v", args)
	}
	if !argvContains(args, "--workdir") {
		t.Errorf("Args must include --workdir: %v", args)
	}
	last := args[len(args)-1]
	if last != prompt("q", false) {
		t.Errorf("Args must end with the built prompt, got %q", last)
	}
}

func TestExploreToolsReadOnly(t *testing.T) {
	cases := []struct {
		vendor       string
		wantTools    bool
		wantReadOnly bool
	}{
		{"copilot", true, true},
		{"codex", false, false},
	}
	for _, c := range cases {
		t.Run(c.vendor, func(t *testing.T) {
			args, err := Args(c.vendor, "/work", "inspect the repository", false, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(args, "--tools"); got != c.wantTools {
				t.Errorf("Args --tools = %v, want %v", got, c.wantTools)
			}
			if got := slices.Contains(args, "--read-only"); got != c.wantReadOnly {
				t.Errorf("Args --read-only = %v, want %v", got, c.wantReadOnly)
			}
			base := agent.RunOptions{Agent: c.vendor}
			options, err := options(base, "inspect the repository", false)
			if err != nil {
				t.Fatal(err)
			}
			if options.Tools != c.wantTools || options.ReadOnly != c.wantReadOnly {
				t.Errorf("Options Tools/ReadOnly = %v/%v, want %v/%v",
					options.Tools, options.ReadOnly, c.wantTools, c.wantReadOnly)
			}
		})
	}
}

func TestExploreRejectsPromptOnlyAgents(t *testing.T) {
	for _, vendor := range []string{"gemini", "opencode"} {
		t.Run(vendor, func(t *testing.T) {
			want := "explore on " + vendor + " needs native read-only support, which it emulates with permission denies; use codex or copilot"
			args, err := Args(vendor, "/work", "inspect", true, true)
			assertExploreRefusal(t, err, want)
			if args != nil {
				t.Fatalf("Args returned runnable arguments: %v", args)
			}
			_, err = options(agent.RunOptions{Agent: vendor}, "inspect", true)
			assertExploreRefusal(t, err, want)
			r := Role{Name: "explore", Agent: vendor}
			legs := Legs(r, Request{Task: "inspect", WorkDir: "/work", Web: true})
			if len(legs) != 1 {
				t.Fatalf("want one refused leg, got %+v", legs)
			}
			if legs[0].Err != want {
				t.Fatalf("leg error = %q; want %q", legs[0].Err, want)
			}
			seen := fakeStream(t)
			rep := Run(context.Background(), nil, r, legs, 1, nil)
			if len(*seen) != 0 || rep.Status != "error" {
				t.Fatalf("refused explore dispatched: calls=%v report=%+v", *seen, rep)
			}
			if env := rep.Legs[0].Envelope; env.Agent != vendor || env.Error == nil || env.Error.Message != want {
				t.Fatalf("refusal envelope lost the refusal or the agent identity: %+v", env)
			}
		})
	}
}

func assertExploreRefusal(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatal("explore accepted a prompt-only agent")
	}
	if err.Error() != want {
		t.Fatalf("explore error = %v; want %q", err, want)
	}
}

func argvContains(args []string, flag string) bool {
	return slices.Contains(args, flag)
}
