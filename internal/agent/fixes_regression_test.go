package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/agent/retry"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestPermissionUnicodePathDeny(t *testing.T) {
	for _, path := range []string{"café.txt", "資料.txt", "🔒.txt"} {
		p := mustPerms(t, "auto", []string{"Read"}, nil, []string{"Read(" + path + ")"})
		if got := p.Decide("Read", path); got != Deny {
			t.Errorf("Read(%q) = %v, want Deny", path, got)
		}
		if got := p.Decide("Read", "public.txt"); got != Allow {
			t.Errorf("Read(public.txt) = %v, want Allow", got)
		}
	}
}

func TestCredentialScrubRegistryFailure(t *testing.T) {
	vaulttest.Use(t)
	vaulttest.Put(t, vault.Overlay, "providers", `{not json`)
	if _, err := providers.Load(); err == nil {
		t.Fatal("fixture must make the provider registry unreadable")
	}
	env := []string{"PATH=/bin", "FOREIGN_KEY=key", "FOREIGN_TOKEN=token", "FOREIGN_SECRET=secret",
		"FOREIGN_PASSWORD=password", "FOREIGN_CREDENTIALS=credentials", "KEPT_TOKEN=kept", "OWN_KEY=own"}
	for _, own := range []bool{false, true} {
		want := []string{"PATH=/bin", "KEPT_TOKEN=kept"}
		ctx := withIsolation(context.Background())
		if own {
			ctx = withOwnProvider(ctx, &providers.Provider{Name: "mine", Auth: providers.Auth{Env: "OWN_KEY"}})
			want = append(want, "OWN_KEY=own")
		}
		if got := scrubIsolated(ctx, env, []string{"KEPT_TOKEN"}); !reflect.DeepEqual(got, want) {
			t.Errorf("own=%v: scrubbed env = %q, want %q", own, got, want)
		}
	}
}

func TestFallbackRetrySinkFailureKeepsHops(t *testing.T) {
	vaulttest.Use(t)
	first := &fakeRunner{name: "codex", emptyAnswer: true, errs: []error{errors.New("boom")}}
	second := &fakeRunner{name: "gemini", errs: []error{nil}}
	rejected := errors.New("retry sink rejected event")
	sink := sinkFunc(func(ev wire.Event) error {
		if ev.Type == wire.EventRetry {
			return rejected
		}
		return nil
	})
	res, err := RunWithFallback(context.Background(), chainOf([]Engine{first, second}), RunOptions{Prompt: "p"}, sink, nil)
	if !errors.Is(err, rejected) {
		t.Fatalf("fallback error = %v, want the retry sink error", err)
	}
	if len(first.seen) != 1 || len(second.seen) != 0 {
		t.Fatalf("dispatch calls = %d, %d; want only codex to have run", len(first.seen), len(second.seen))
	}
	if len(res.Hops) != 1 || res.Hops[0].Agent != "codex" {
		t.Fatalf("hops = %+v, want the attempted codex hop", res.Hops)
	}
	if got := wire.RanOn(res); got == "" || !strings.Contains(got, "codex") {
		t.Fatalf("RanOn = %q, want the attempted codex dispatch", got)
	}
}

func TestRunAuthRecoveryUsesCodexPinnedProvider(t *testing.T) {
	vaulttest.Use(t)
	fastDefaultBackoff(t)
	marker := filepath.Join(t.TempDir(), "refreshed")
	installRecoveryProfiles(t, marker)
	t.Setenv(providers.EnvSessionProfile, "regression-session")
	vaulttest.SetRecord(t, "codex", "profile", "regression-pinned")
	r := &fakeRunner{name: "codex", emptyAnswer: true, errs: []error{errors.New("401 Unauthorized"), nil}}
	res, err := Run(context.Background(), r, RunOptions{Prompt: "p"}, wire.Discard, nil)
	if err != nil || res.Transport.Attempts != 2 {
		t.Fatalf("Run = %+v, %v; want auth recovery followed by a second attempt", res, err)
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "regression-pinned" {
		t.Fatalf("refreshed provider = %q, %v; want Codex's pin, not the session", got, err)
	}
}

func installRecoveryProfiles(t *testing.T, marker string) {
	t.Helper()
	var profiles []string
	for _, name := range []string{"regression-session", "regression-pinned"} {
		command := fmt.Sprintf("printf %s > %q; printf test-token", name, marker)
		profiles = append(profiles, fmt.Sprintf(`%q:{"protocol":"openai","base_url":"http://127.0.0.1:1","auth":{"type":"key","command":%q},"models":{"opus":"gpt-6.1-sol"}}`, name, command))
	}
	vaulttest.Put(t, vault.Overlay, "providers", `{"providers":{"openai":{`+strings.Join(profiles, ",")+`}}}`)
}

func TestLoadSchemaRejectsInvalidTools(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for _, tools := range []string{"false", "no"} {
		file := filepath.Join(t.TempDir(), "restricted.md")
		if err := writeFile(file, "---\nname: restricted\ntools: "+tools+"\n---\nRestricted prompt.\n"); err != nil {
			t.Fatal(err)
		}
		got, err := LoadSchema(file)
		if err == nil || got != nil {
			t.Fatalf("tools: %s loaded as %+v, %v; want rejection, not inherited tools", tools, got, err)
		}
		if !strings.Contains(err.Error(), "tools must be") {
			t.Fatalf("tools: %s error = %v, want a tools validation error", tools, err)
		}
	}
}

func TestFailFastPreservesProgressedFallback(t *testing.T) {
	for _, message := range []string{"429 Too Many Requests", "insufficient_quota: you exceeded your current quota"} {
		t.Run(message, func(t *testing.T) {
			vaulttest.Use(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = retry.WithFailFast(ctx, cancel)
			first := &fakeRunner{name: "codex", write: "partial progress", errs: []error{errors.New(message)}}
			second := &fakeRunner{name: "gemini", errs: []error{nil}}
			res, err := RunWithFallback(ctx, chainOf([]Engine{first, second}), RunOptions{Prompt: "p"}, wire.Discard, nil)
			if err != nil || ctx.Err() != nil || res.Agent != "gemini" {
				t.Fatalf("fallback = %+v, %v; context = %v; want gemini success without fail-fast cancellation", res, err, ctx.Err())
			}
			if len(first.seen) != 1 || len(second.seen) != 1 {
				t.Fatalf("dispatch calls = %d, %d; want one attempt per dispatch", len(first.seen), len(second.seen))
			}
		})
	}
}

func TestParseAgentsRejectsNullFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "agents.json")
	if err := writeFile(file, "null\n"); err != nil {
		t.Fatal(err)
	}
	got, err := ParseAgents(file)
	if err == nil || got != nil {
		t.Fatalf("ParseAgents(null file) = %v, %v; want rejection, not empty definitions", got, err)
	}
	if !strings.Contains(err.Error(), "--agents: top-level value must be a JSON object") {
		t.Fatalf("ParseAgents(null file) error = %v, want an object-shape error", err)
	}
	if err := writeFile(file, "{}\n"); err != nil {
		t.Fatal(err)
	}
	if got, err := ParseAgents(file); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("ParseAgents(empty object) = %v, %v; want valid empty definitions", got, err)
	}
}

func TestLookupSchemaPinIDsAreIndependent(t *testing.T) {
	s, ok := LookupSchema("opencode")
	if !ok || len(s.CLI.PinIDs) == 0 {
		t.Fatal("opencode must expose at least one extra pin ID")
	}
	original := s.CLI.PinIDs[0]
	t.Cleanup(func() { s.CLI.PinIDs[0] = original })
	s.CLI.PinIDs[0] = "bogus"
	if err := ValidAgentPin(nil, "opencode", "bogus"); err == nil {
		t.Fatal("mutating lookup PinIDs made bogus a valid opencode pin")
	}
	if err := ValidAgentPin(nil, "opencode", original); err != nil {
		t.Fatalf("mutating lookup PinIDs invalidated original pin %q: %v", original, err)
	}
}
