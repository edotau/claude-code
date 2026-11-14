package ask

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/roles"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/hosttest"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Given a detached leg whose spawn fails (the test binary is never relaunched), Then its record leaves running.
func TestDispatchDetachSpawnFailureFinishesRecord(t *testing.T) {
	vaulttest.UseFreshKey(t)
	legs := []roles.Leg{{Label: "a", Opts: agent.RunOptions{Agent: "claude", Prompt: "p"}}}
	var code int
	hosttest.CaptureOutput(t, func() int {
		code = dispatchDetach(context.Background(), agent.NewClient(nil, nil, nil), legs, helptext.Text)
		return code
	})
	if code != 1 {
		t.Fatalf("dispatchDetach = %d, want 1", code)
	}
	recs := agent.RecentRecords(5)
	if len(recs) != 1 || recs[0].Status != "error" || recs[0].Error == "" {
		t.Errorf("records = %+v, want one errored record with the cause", recs)
	}
}

func TestUnsupportedKeyNamesTheKeyDispatchCannotHonour(t *testing.T) {
	for key, ov := range map[string]agentconfig.Overlay{"model": {Model: "m"}, "effort": {Effort: "high"}, "host": {Host: "h"}, "": {Profile: "dev"}} {
		if got := unsupportedKey(ov); got != key {
			t.Errorf("unsupportedKey(%+v) = %q, want %q", ov, got, key)
		}
	}
}

// An empty overlay names nothing; one with several unsupported keys still names one of them, never "".
func TestUnsupportedKeyEmptyAndSeveral(t *testing.T) {
	for name, tc := range map[string]struct {
		ov   agentconfig.Overlay
		want []string
	}{
		"empty":   {agentconfig.Overlay{}, []string{""}},
		"two":     {agentconfig.Overlay{Effort: "high", Host: "h"}, []string{"effort", "host"}},
		"all":     {agentconfig.Overlay{Model: "m", Effort: "high", Host: "h"}, []string{"model", "effort", "host"}},
		"allowed": {agentconfig.Overlay{Profile: "dev", Fallback: "none"}, []string{""}},
	} {
		if got := unsupportedKey(tc.ov); !slices.Contains(tc.want, got) {
			t.Errorf("%s: unsupportedKey = %q, want one of %q", name, got, tc.want)
		}
	}
}

func TestDispatchReportWriteFailureExitsOne(t *testing.T) {
	vaulttest.UseFreshKey(t)
	vaulttest.SetRecord(t, "codex", "profile", "chatgpt")
	pathstest.FakeVendor(t, "codex", `[ "$1" = login ] && { echo "Logged in using ChatGPT"; exit 0; }`+"\ncat >/dev/null; echo successful leg")
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if os.IsNotExist(err) {
		t.Skip("requires /dev/full")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := full.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, format := range []string{helptext.Text, helptext.JSON} {
		t.Run(format, func(t *testing.T) {
			checkDispatchReportWriteFailure(t, full, format)
		})
	}
}

// A consumer that stops draining ends the leg (and frees the gate) instead of blocking its send forever.
func TestStreamLegAbandonedConsumerReleasesGate(t *testing.T) {
	armRateFixture(t, nil, nil)
	orig := armStreamStall
	armStreamStall = 50 * time.Millisecond
	t.Cleanup(func() { armStreamStall = orig })
	a := &armRetry{agentName: "codex", fallback: router.FallbackNone, tried: []string{"dev"}}
	o := agent.RunOptions{Agent: "codex", Prompt: "go", Fallback: router.FallbackNone}
	done := make(chan struct{})
	go func() {
		a.streamLeg(t.Context(), newClient(), o, make(chan wire.Event)) // unbuffered, never read
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("streamLeg still blocked on an undrained channel")
	}
}

func checkDispatchReportWriteFailure(t *testing.T, full *os.File, format string) {
	t.Helper()
	role := roles.Role{Name: "explore", Agent: "codex"} // explore refuses opencode and gemini (prompt-only read-only)
	request := roles.Request{Task: "test report", Parts: []string{"test leg"}, WorkDir: t.TempDir()}
	run := func() int { return dispatchRun(t.Context(), role, request, 1, format, false) }
	code, stdout, stderr := hosttest.CaptureOutput(t, run)
	if code != 0 || !strings.Contains(stdout, "successful leg") {
		t.Fatalf("successful dispatch control: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, _, stderr = hosttest.CaptureOutput(t, func() int {
		original := os.Stdout
		os.Stdout = full
		defer func() { os.Stdout = original }()
		return run()
	})
	if code != 1 || !strings.Contains(stderr, "dispatch: write report:") || !strings.Contains(stderr, "no space left on device") {
		t.Fatalf("lost report: exit %d, stderr %q; want exit 1 naming report write failure", code, stderr)
	}
}

// profileFixture isolates the vault and seeds provider acme (no opencodeID).
func profileFixture(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	doc := `{` + acmeProvider + `}`
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, "providers", doc) }); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}
}

func TestClientPrepareHubNativeFallback(t *testing.T) {
	profileFixture(t)
	t.Setenv("HOME", t.TempDir())
	pathstest.FakeVendor(t, "opencode", `printf '%s\n' '{"type":"text","part":{"text":"ok"}}'`)
	client := newClient()
	for _, profile := range []string{"acme", "chatgpt"} {
		p, err := client.Prepare(context.Background(), agent.RunOptions{
			Agent: "opencode", Provider: profile, Prompt: "p", Fallback: router.FallbackAgents}, nil)
		if err != nil {
			t.Fatalf("Prepare(%s) = %v", profile, err)
		}
		res, err := client.Exec(context.Background(), p, nil, agent.RunRecordedOpts{})
		if len(res.Hops) != 1 {
			t.Fatalf("%s: want one dispatch, got %+v, err %v", profile, res.Hops, err)
		}
		if profile == "acme" && (err != nil || res.Answer != "ok") {
			t.Fatalf("native profile did not run: %+v, %v", res, err)
		}
		if profile == "chatgpt" && (err == nil || !strings.Contains(err.Error(), "opencode has no openai login")) {
			t.Fatalf("hub profile did not check its login: %+v, %v", res, err)
		}
	}
}

// A cancelled batch prepares nothing and never refreshes quota.
func TestClientPrepareHonorsCancellation(t *testing.T) {
	refreshes := 0
	prev := RouteRefresh
	RouteRefresh = func(context.Context, string) bool { refreshes++; return false }
	t.Cleanup(func() { RouteRefresh = prev })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newClient().Prepare(ctx, agent.RunOptions{Agent: "opencode", Prompt: "p", Fallback: router.FallbackAgents}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if refreshes != 0 {
		t.Errorf("%d quota refreshes ran under a cancelled ctx", refreshes)
	}
}

// An explicit --profile with no --model runs the classifier's alias, as the pinned route does.
func TestClientPrepareProfileTakesTierSlot(t *testing.T) {
	profileFixture(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	client := newClient()
	for _, profile := range []string{"chatgpt", "acme"} {
		p, err := client.Prepare(context.Background(), agent.RunOptions{
			Agent: "opencode", Provider: profile, Prompt: "pong", Fallback: router.FallbackAgents}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, _ := client.Exec(context.Background(), p, nil, agent.RunRecordedOpts{})
		if len(res.Hops) != 1 {
			t.Fatalf("%s: hops = %+v, want one unavailable dispatch", profile, res.Hops)
		}
		if want := (router.RuleClassifier{}).Alias("pong"); res.Hops[0].Model != want {
			t.Errorf("%s: dispatch model = %q, want alias %q", profile, res.Hops[0].Model, want)
		}
	}
}

// An explicit --profile is one dispatch for every agent even with --fallback all: nothing can silently take over.
func TestClientPrepareProfileIsOneDispatchUnderFallbackAll(t *testing.T) {
	profileFixture(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	client := newClient()
	for _, name := range []string{"codex", "claude", "opencode", "copilot", "gemini"} {
		p, err := client.Prepare(context.Background(), agent.RunOptions{
			Agent: name, Provider: "acme", Prompt: "p", Fallback: router.FallbackAll}, nil)
		if err != nil {
			t.Fatalf("%s: Prepare = %v", name, err)
		}
		res, err := client.Exec(context.Background(), p, nil, agent.RunRecordedOpts{})
		if err == nil || len(res.Hops) != 1 {
			t.Errorf("%s: %d hops, err %v; want one failed dispatch without fallback", name, len(res.Hops), err)
		}
	}
}

// Every flag RegisterRouteFlags defines comes back out of Args, so a new route flag cannot be dropped silently.
func TestRouteFlagsArgsCoverEveryRouteFlag(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	route := RegisterRouteFlags(fs)
	var args []string
	fs.VisitAll(func(f *flag.Flag) {
		val := "x"
		if f.Name == "mode" {
			val = "plan"
		}
		args = append(args, "--"+f.Name, val)
	})
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	got, err := route.Args()
	if err != nil {
		t.Fatal(err)
	}
	fs.VisitAll(func(f *flag.Flag) {
		if !slices.Contains(got, "--"+f.Name) {
			t.Errorf("route flag --%s is not forwarded by Args: %q", f.Name, got)
		}
	})
	if _, err := (&RouteFlags{f: askFlags{overlay: "-"}}).Args(); err == nil {
		t.Error("--overlay - must be refused: stdin carries the batch lines")
	}
}
