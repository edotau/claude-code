package vendors

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edotau/claude-code/internal/copilotreg"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
)

func TestCopilotRegistryRefusedOnDarwin(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	old := copilotGOOS
	t.Cleanup(func() { copilotGOOS = old })
	copilotGOOS = "darwin"

	_, err := PrepareCopilotHome(true)
	if err == nil || !strings.Contains(err.Error(), "--direct") || !strings.Contains(err.Error(), "follow-up") {
		t.Fatalf("PrepareCopilotHome on darwin: %v", err)
	}
	cr, err := ConfigureCopilotRegistry(context.Background(), nil, providers.Target{}, nil, true)
	if err == nil || cr.Token != "" {
		t.Fatalf("ConfigureCopilotRegistry on darwin: token %q err %v", cr.Token, err)
	}
	copilotGOOS = "linux"
	if err := checkCopilotRegistryOS(); err != nil {
		t.Fatalf("linux refused: %v", err)
	}
}

// --print-env (dry) reads state only: it neither starts the router nor creates the runtime dir or private home.
func TestCopilotRegistryDryCreatesNothing(t *testing.T) {
	cfg, run := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("XDG_RUNTIME_DIR", run)
	t.Setenv("HOME", t.TempDir())
	oldD, oldE, oldR := discoverModels, ensureRouter, runningRouter
	t.Cleanup(func() { discoverModels, ensureRouter, runningRouter = oldD, oldE, oldR })
	discoverModels = func(context.Context, *providers.Registry, *providers.Provider) ([]copilotreg.Endpoint, error) {
		var ep copilotreg.Endpoint
		ep.ID = "databricks-claude-sonnet-5-5"
		return []copilotreg.Endpoint{ep}, nil
	}
	ensureRouter = func(context.Context) (string, error) { t.Fatal("dry ensured (may start) the router"); return "", nil }
	runningRouter = func(context.Context) (proxy.State, bool) { return proxy.State{}, false }

	tgt := providers.Target{Provider: &providers.Provider{Name: "dbx"}}
	cr, err := ConfigureCopilotRegistry(context.Background(), nil, tgt, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if cr.Token != "" || len(cr.Env) == 0 {
		t.Fatalf("dry launch: token %q env %v", cr.Token, cr.Env)
	}
	for _, d := range []string{cfg, run} {
		if ents, _ := os.ReadDir(d); len(ents) != 0 {
			t.Errorf("dry run created entries under %s: %v", d, ents)
		}
	}
}

// Concurrent launches initialise the shared private home without ENOENT/EEXIST aborts.
func TestPrepareCopilotHomeConcurrent(t *testing.T) {
	cfg, userHome := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("COPILOT_HOME", "")
	t.Setenv("HOME", userHome)
	for _, n := range copilotSeedLinks {
		if err := os.MkdirAll(filepath.Join(userHome, ".copilot", n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 32 {
		wg.Go(func() {
			if _, err := PrepareCopilotHome(false); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if _, err := os.Readlink(filepath.Join(copilotRegistryHome(), copilotSeedLinks[0])); err != nil {
		t.Errorf("link missing after concurrent init: %v", err)
	}
}
