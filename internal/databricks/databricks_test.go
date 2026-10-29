package databricks

import (
	"bytes"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/providers"
)

// call is one recorded spawn of the child.
type call struct {
	bin  string
	argv []string
	env  []string
}

// fakeChild records every spawn and answers the smoke prompt from its script.
type fakeChild struct {
	calls []call
	smoke string // stdout for the smoke prompt
	code  int
	err   error
}

func (f *fakeChild) spawn(bin string, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.calls = append(f.calls, call{bin: bin, argv: slices.Clone(argv), env: slices.Clone(env)})
	if f.err != nil {
		return 0, f.err
	}
	_, _ = io.WriteString(stdout, f.smoke)
	return f.code, nil
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

// threeProfiles defines three opaque Databricks profiles so the plan tests never read the real vault.
const threeProfiles = `{"providers":{"databricks":{"aa":{"preset":"databricks",` + dbxHost + `},"bb":{"preset":"databricks",` +
	dbxHost + `},"cc":{"preset":"databricks",` + dbxHost + `}}}}`

// opusDefault is the default model for the seeded rows: the opus alias at 1M, as the row spells it.
func opusDefault(t *testing.T, name string) string {
	t.Helper()
	reg, err := providers.Load()
	if err != nil || reg.Providers[name] == nil {
		t.Fatalf("profile %q: %v", name, err)
	}
	return reg.Providers[name].WireModel(aliasID(models.Opus))
}

// TestPlanTable pins the default model, profile selection, passthrough order and --profile forms.
func TestPlanTable(t *testing.T) {
	seedOverlay(t, threeProfiles)
	t.Setenv(providers.EnvSessionProfile, "")
	def := opusDefault(t, "bb")
	tests := []struct {
		name     string
		args     []string
		wantProv string
		wantMod  string
		wantRest []string
	}{
		{"model only", []string{"sonnet"}, "", "sonnet", nil},
		{"--profile bb", []string{"--profile", "bb"}, "bb", def, nil},
		{"--profile after flags", []string{"--print", "hi", "--profile", "cc"}, "cc", def, []string{"--print", "hi"}},
		{"--profile= form", []string{"--profile=bb"}, "bb", def, nil},
		{"model + --profile=cc", []string{"haiku", "--profile=cc"}, "cc", "haiku", nil},
		{"rest order kept", []string{"--profile", "cc", "ask", "hello world"}, "cc", def, []string{"ask", "hello world"}},
		{"flags-first keeps default model", []string{"--print", "hi", "--profile=bb"}, "bb", def, []string{"--print", "hi"}},
		{"only MODEL is taken", []string{"sonnet", "haiku", "--profile=bb"}, "bb", "sonnet", []string{"haiku"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := plan(tt.args)
			if err != nil {
				t.Fatalf("plan(%q) error: %v", tt.args, err)
			}
			want := tt.wantProv
			if want == "" {
				want = providers.DatabricksProfiles(mustLoad(t))[0]
			}
			if p.Provider != want || p.Model != tt.wantMod && tt.wantProv != "" || p.Model == "" {
				t.Errorf("got profile %q model %q, want %q %q", p.Provider, p.Model, want, tt.wantMod)
			}
			if !slices.Equal(p.Rest, tt.wantRest) {
				t.Errorf("rest = %q, want %q", p.Rest, tt.wantRest)
			}
		})
	}
}

func mustLoad(t *testing.T) *providers.Registry {
	t.Helper()
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// TestResolveProfileOrder pins explicit, then session, then the CLAUDE_PROFILE pin, then the first Databricks profile.
func TestResolveProfileOrder(t *testing.T) {
	seedOverlay(t, threeProfiles)
	reg := mustLoad(t)
	first := providers.DatabricksProfiles(reg)[0]
	t.Setenv(providers.EnvSessionProfile, "")
	if got := resolveProfile(reg, "zz"); got != "zz" {
		t.Errorf("explicit = %q, want zz", got)
	}
	if got := resolveProfile(reg, ""); got != first {
		t.Errorf("no pin = %q, want the first profile %q", got, first)
	}
	if err := providers.SetPins(map[string]string{providers.PinProfile: "cc"}); err != nil {
		t.Fatal(err)
	}
	if got := resolveProfile(reg, ""); got != "cc" {
		t.Errorf("pinned = %q, want cc", got)
	}
	t.Setenv(providers.EnvSessionProfile, "bb")
	if got := resolveProfile(reg, ""); got != "bb" {
		t.Errorf("session = %q, want bb", got)
	}
	t.Setenv(providers.EnvSessionProfile, "not-databricks") // shadows the pin of the same name
	if got := resolveProfile(reg, ""); got != first {
		t.Errorf("non-Databricks session = %q, want the first profile %q", got, first)
	}
}

// TestPlanCheck pins the leading check token: only the first arg turns check on.
func TestPlanCheck(t *testing.T) {
	seedOverlay(t, threeProfiles)
	t.Setenv(providers.EnvSessionProfile, "bb")
	tests := []struct {
		name      string
		args      []string
		wantCheck bool
		wantProv  string
		wantRest  []string
	}{
		{"check alone", []string{"check"}, true, "bb", nil},
		{"check model profile", []string{"check", "haiku", "--profile", "cc"}, true, "cc", nil},
		{"check not first is a rest arg", []string{"sonnet", "check"}, false, "bb", []string{"check"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := plan(tt.args)
			if err != nil {
				t.Fatalf("plan(%q) error: %v", tt.args, err)
			}
			if p.Check != tt.wantCheck || p.Provider != tt.wantProv {
				t.Errorf("check=%v profile=%q, want %v %q", p.Check, p.Provider, tt.wantCheck, tt.wantProv)
			}
			if !slices.Equal(p.Rest, tt.wantRest) {
				t.Errorf("rest = %q, want %q", p.Rest, tt.wantRest)
			}
		})
	}
}

// TestPlanProfileErrors pins that a name outside the Databricks profiles is refused, including no value.
func TestPlanProfileErrors(t *testing.T) {
	seedOverlay(t, threeProfiles)
	t.Setenv(providers.EnvSessionProfile, "")
	tests := map[string][]string{
		"unknown":              {"--profile", "staging"},
		"= unknown":            {"--profile=staging"},
		"no value":             {"--profile"},
		"= (empty)":            {"--profile="},
		"eats a flag":          {"--profile", "--print"},
		"after model, unknown": {"sonnet", "--profile", "stage"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			p, err := plan(args)
			if name == "no value" || name == "= (empty)" {
				if err != nil { // an empty name falls back to the default profile
					t.Fatalf("plan(%q) error = %v", args, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "not a Databricks profile") {
				t.Fatalf("plan(%q) = %+v, %v; want a profile error", args, p, err)
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
	seedOverlay(t, threeProfiles)
	tests := [][]string{{"sonnet", "--print", "-h"}, {"--profile", "bb", "-h"}}
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

// TestRunProfileErrorExit2 pins exit 2 and no child start for an unknown profile.
func TestRunProfileErrorExit2(t *testing.T) {
	seedOverlay(t, threeProfiles)
	f := &fakeChild{}
	useFake(t, f)
	code, _, stderr := runCaptured(t, func() int { return Run([]string{"--profile", "staging"}) })
	if code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "not a Databricks profile") {
		t.Errorf("stderr = %q, want profile error", stderr)
	}
	if len(f.calls) != 0 {
		t.Errorf("bad profile launched the child")
	}
}

// TestLaunchArgvNoShellAndBetaEnv pins the list-argv launch; the beta opt-out is covered in dialect_test.go.
func TestLaunchArgvNoShellAndBetaEnv(t *testing.T) {
	seedOverlay(t, threeProfiles)
	f := &fakeChild{}
	useFake(t, f)
	if code := Run([]string{"sonnet", "--profile", "cc", "--print", "hi"}); code != 0 {
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
	want := []string{"--profile", "cc", "--model", "sonnet", "--print", "hi"}
	if !slices.Equal(c.argv, want) {
		t.Errorf("argv = %q, want %q", c.argv, want)
	}
	for _, shell := range []string{"sh", "bash", "-c"} {
		if slices.Contains(c.argv, shell) || strings.HasSuffix(c.bin, shell) {
			t.Errorf("launch looks shell-wrapped via %q", shell)
		}
	}
}

// TestLaunchPropagatesExitAndStartError pins the child's exit code and exit 2 on a start failure.
func TestLaunchPropagatesExitAndStartError(t *testing.T) {
	seedOverlay(t, threeProfiles)
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
	seedOverlay(t, threeProfiles)
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

// A removed flag exits 2 with its hint before any subverb, probe or child launch.
func TestRunRemovedFlagsExit2NoLaunch(t *testing.T) {
	for _, args := range [][]string{
		{"--env", "qa"}, {"check", "--env", "qa"}, {"check", "--provider=x"}, {"--tier", "opus"}, {"--router"}, {"check", "--1m"},
	} {
		f := &fakeChild{smoke: "OK\n"}
		useFake(t, f)
		code, stdout, stderr := runCaptured(t, func() int { return Run(args) })
		if code != 2 || !strings.Contains(stderr, "--") || strings.Contains(stdout, "PASS") {
			t.Errorf("Run(%q) = %d, stdout %q, stderr %q; want exit 2 with a hint", args, code, stdout, stderr)
		}
		if len(f.calls) != 0 {
			t.Errorf("Run(%q) launched the child: %v", args, f.calls)
		}
	}
}
