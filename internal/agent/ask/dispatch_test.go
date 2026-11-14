package ask

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// Given a spawn that failed, Then failSpawn finishes the record with the cause so it never shows as running.
func TestFailSpawnFinishesRecord(t *testing.T) {
	vaulttest.UseFreshKey(t)
	rec := agent.NewRecord("claude", agent.RunOptions{Agent: "claude", Prompt: "p"}, time.Now())
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("spawn refused")
	if err := failSpawn(&rec, cause); !errors.Is(err, cause) {
		t.Fatalf("failSpawn = %v, want the cause", err)
	}
	recs := agent.RecentRecords(5)
	if len(recs) != 1 || recs[0].Status != "error" || recs[0].Error == "" {
		t.Errorf("records = %+v, want one errored record with the cause", recs)
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
