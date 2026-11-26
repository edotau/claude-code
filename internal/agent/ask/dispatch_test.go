package ask

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/platform/paths/pathstest"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/platform/vault/vaulttest"
	"github.com/edotau/claude-code/internal/routing/route"
	"github.com/edotau/claude-code/internal/routing/router"
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
			Agent: "opencode", Provider: profile, Prompt: "p", Fallback: route.FallbackAgents}, nil)
		if profile == "chatgpt" { // a typed row the agent cannot use is refused before any dispatch
			if err == nil || !strings.Contains(err.Error(), "cannot use") {
				t.Fatalf("hub profile Prepare = %v, want a refusal naming the codex login", err)
			}
			continue
		}
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
	if _, err := newClient().Prepare(ctx, agent.RunOptions{Agent: "opencode", Prompt: "p", Fallback: route.FallbackAgents}, nil); !errors.Is(err, context.Canceled) {
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
	for _, profile := range []string{"acme"} {
		p, err := client.Prepare(context.Background(), agent.RunOptions{
			Agent: "opencode", Provider: profile, Prompt: "pong", Fallback: route.FallbackAgents}, nil)
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
			Agent: name, Provider: "acme", Prompt: "p", Fallback: route.FallbackAll}, nil)
		if name == "codex" || name == "gemini" { // acme has no responses route or custom base URL: refused before dispatch
			if err == nil {
				t.Errorf("%s: Prepare on acme succeeded, want a refusal", name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: Prepare = %v", name, err)
		}
		res, err := client.Exec(context.Background(), p, nil, agent.RunRecordedOpts{})
		if err == nil || len(res.Hops) != 1 {
			t.Errorf("%s: %d hops, err %v; want one failed dispatch without fallback", name, len(res.Hops), err)
		}
	}
}

// RouteFlags.Args folds --fallback into the single --overlay document and refuses --overlay -.
func TestRouteFlagsArgsCoverEveryRouteFlag(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	route := RegisterRouteFlags(fs)
	values := map[string]string{"overlay": `{"runner":"cli"}`, "fallback": "none"}
	var args []string
	fs.VisitAll(func(f *flag.Flag) {
		value, ok := values[f.Name]
		if !ok {
			t.Fatalf("new route flag --%s needs a forwarding assertion", f.Name)
		}
		args = append(args, "--"+f.Name, value)
	})
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	got, err := route.Args()
	if err != nil {
		t.Fatal(err)
	}
	var ov struct{ Runner, Fallback string }
	if len(got) != 2 || got[0] != "--overlay" || json.Unmarshal([]byte(got[1]), &ov) != nil || ov.Runner != "cli" || ov.Fallback != "none" {
		t.Fatalf("route.Args() = %q, want one --overlay carrying runner and fallback", got)
	}
	if _, err := (&RouteFlags{f: askFlags{overlay: "-"}}).Args(); err == nil {
		t.Error("--overlay - must be refused: stdin carries the batch lines")
	}
}
