package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

const acmeProvider = `"providers":{"openai":{"acme":{"protocol":"openai","base_url":"https://acme.example/v1","auth":{"type":"none"}}}}`

// profileFixture isolates the vault and seeds provider acme (no opencodeID) plus, when active is set, a "work" set.
func profileFixture(t *testing.T, active string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HARNESS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	doc := `{` + acmeProvider + `}`
	if active != "" {
		doc = `{` + acmeProvider + `,"profiles":{"active":"` + active + `","sets":{"work":{
		  "codex":{"account":"acme","model":"acme-large","effort":"high"}}}}}`
	}
	if err := vault.Update(func(tx *vault.Tx) error { return tx.Put(vault.Overlay, "providers", doc) }); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}
}

func TestClientPrepareHubNativeFallback(t *testing.T) {
	profileFixture(t, "")
	t.Setenv("HOME", t.TempDir())
	pathstest.FakeVendor(t, "opencode", `printf '%s\n' '{"type":"text","part":{"text":"ok"}}'`)
	client := newClient()
	for _, profile := range []string{"acme", "chatgpt"} {
		p, err := client.Prepare(context.Background(), agent.RunOptions{
			Agent: "opencode", Provider: profile, Prompt: "p", Fallback: router.FallbackAgents}, nil)
		if err != nil {
			t.Fatalf("Prepare(%s) = %v", profile, err)
		}
		res, err := client.Exec(context.Background(), p, nil, agent.DispatchOpts{})
		if len(res.Hops) != 1 {
			t.Fatalf("%s: want one leg, got %+v, err %v", profile, res.Hops, err)
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
	prev := routeRefresh
	routeRefresh = func(context.Context, string) bool { refreshes++; return false }
	t.Cleanup(func() { routeRefresh = prev })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newClient().Prepare(ctx, agent.RunOptions{Agent: "opencode", Prompt: "p", Fallback: router.FallbackAgents}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if refreshes != 0 {
		t.Errorf("%d quota refreshes ran under a cancelled ctx", refreshes)
	}
}

// An explicit --profile with no --model runs the classifier's tier slot, as the pinned route does.
func TestClientPrepareProfileTakesTierSlot(t *testing.T) {
	profileFixture(t, "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	client := newClient()
	for _, profile := range []string{"chatgpt", "acme"} {
		p, err := client.Prepare(context.Background(), agent.RunOptions{
			Agent: "opencode", Provider: profile, Prompt: "pong", Fallback: router.FallbackAgents}, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, _ := client.Exec(context.Background(), p, nil, agent.DispatchOpts{})
		if len(res.Hops) != 1 {
			t.Fatalf("%s: hops = %+v, want one unavailable leg", profile, res.Hops)
		}
		if want := (router.RuleClassifier{}).Tier("pong"); res.Hops[0].Model != want {
			t.Errorf("%s: leg model = %q, want tier %q", profile, res.Hops[0].Model, want)
		}
	}
}

// An explicit --profile is one leg for every agent even with --fallback all: nothing can silently take over.
func TestClientPrepareProfileIsOneLegUnderFallbackAll(t *testing.T) {
	profileFixture(t, "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	client := newClient()
	for _, name := range []string{"codex", "claude", "opencode", "copilot", "gemini"} {
		p, err := client.Prepare(context.Background(), agent.RunOptions{
			Agent: name, Provider: "acme", Prompt: "p", Fallback: router.FallbackAll}, nil)
		if err != nil {
			t.Fatalf("%s: Prepare = %v", name, err)
		}
		res, err := client.Exec(context.Background(), p, nil, agent.DispatchOpts{})
		if err == nil || len(res.Hops) != 1 {
			t.Errorf("%s: %d hops, err %v; want one failed leg without fallback", name, len(res.Hops), err)
		}
	}
}

// A batch task's own profile reaches the request and resolves to one leg, under the default --fallback all.
func TestBatchTaskProfileIsOneLeg(t *testing.T) {
	profileFixture(t, "")
	line := agent.ParsedLine{N: 1, Task: agent.TaskLine{Prompt: "p", Profile: "acme"}}
	bt, err := batchTask(line, batchOpts{agent: "codex", fallback: router.FallbackAll}, agent.RunOptions{}, false, t.TempDir(), map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if bt.Req.Provider != "acme" {
		t.Fatalf("task request provider = %q, want acme", bt.Req.Provider)
	}
	p, err := bt.Prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	res, err := newClient().Exec(context.Background(), p, nil, agent.DispatchOpts{})
	if err == nil || len(res.Hops) != 1 {
		t.Fatalf("Exec: %d hops, err %v; want one unavailable leg", len(res.Hops), err)
	}
	bad := agent.ParsedLine{N: 2, Task: agent.TaskLine{Prompt: "p", Profile: "no-such-profile"}}
	bt, err = batchTask(bad, batchOpts{agent: "codex", fallback: router.FallbackAll}, agent.RunOptions{}, false, t.TempDir(), map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bt.Prepare(context.Background()); err == nil {
		t.Error("unknown batch profile prepared a chain, want an error before any run")
	}
}
