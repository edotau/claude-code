package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/desktop"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func desktopCLIEnv(t *testing.T) string {
	t.Helper()
	vaulttest.Use(t)
	root := t.TempDir()
	oldRoot, oldHelper, oldEnsure := desktopRoot, desktopHelper, desktopEnsure
	desktopRoot = func() (string, error) { return root, nil }
	desktopHelper = func(p string) (string, []string, bool, error) {
		return "/opt/claude-code", []string{"gateway", "token", "--profile", p}, false, nil
	}
	desktopEnsure = func(context.Context) (string, error) { return "http://127.0.0.1:19999", nil }
	t.Cleanup(func() { desktopRoot, desktopHelper, desktopEnsure = oldRoot, oldHelper, oldEnsure })
	return root
}

func TestConfigureDesktopWritesAndReverts(t *testing.T) {
	root := desktopCLIEnv(t)
	code, _, stderr := captureOutput(t, func() int {
		return cmdConfigure([]string{"desktop", "--profile", "anthropic"})
	})
	if code != 0 {
		t.Fatalf("configure exit %d: %s", code, stderr)
	}
	raw, err := os.ReadFile(filepath.Join(root, "configLibrary", desktop.ProfileID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var profile map[string]any
	if err := json.Unmarshal(raw, &profile); err != nil {
		t.Fatal(err)
	}
	if profile["inferenceGatewayBaseUrl"] != "http://127.0.0.1:19999/p/anthropic" {
		t.Fatalf("wrong base URL: %s", raw)
	}
	if headers, ok := profile["inferenceCustomHeaders"].(map[string]any); !ok || headers["X-Claude-Code-Agent"] != "desktop" {
		t.Fatalf("missing attribution: %s", raw)
	}
	code, _, stderr = captureOutput(t, func() int { return cmdConfigure([]string{"desktop", "--revert"}) })
	if code != 0 {
		t.Fatalf("revert exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "configLibrary", desktop.ProfileID+".json")); !os.IsNotExist(err) {
		t.Fatalf("revert retained profile: %v", err)
	}
}

func TestConfigureDesktopDryRunDoesNotStartProxy(t *testing.T) {
	root := desktopCLIEnv(t)
	desktopEnsure = func(context.Context) (string, error) { t.Fatal("dry-run started proxy"); return "", nil }
	t.Setenv("HARNESS_HEADERS", "X-Private: sensitive-header")
	code, stdout, stderr := captureOutput(t, func() int {
		return cmdConfigure([]string{"desktop", "--profile", "anthropic", "--dry-run"})
	})
	if code != 0 || strings.Contains(stdout+stderr, "sensitive-header") {
		t.Fatalf("dry-run exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, desktop.ProfileID+".json") || !strings.Contains(stdout, "_meta.json") {
		t.Fatalf("dry-run missing target paths: %s", stdout)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("dry-run wrote files: %v, %v", entries, err)
	}
}

func TestConfigureDesktopValidation(t *testing.T) {
	root := desktopCLIEnv(t)
	for _, args := range [][]string{
		{"desktop", "--profile", "no-such-profile"},
		{"desktop", "--revert", "--profile", "anthropic"},
		{"desktop", "extra"},
		{"desktop", "--restore"},
		{"desktop", "--profile", "max"},
	} {
		code, stdout, _ := captureOutput(t, func() int { return cmdConfigure(args) })
		if code == 0 || stdout != "" {
			t.Errorf("%v: exit %d stdout %q", args, code, stdout)
		}
	}
	desktopRoot = func() (string, error) { return filepath.Join(root, "never-opened"), nil }
	code, _, stderr := captureOutput(t, func() int { return cmdConfigure([]string{"desktop", "--dry-run"}) })
	if code == 0 || !strings.Contains(stderr, "open Claude Desktop once") {
		t.Fatalf("missing root: exit %d stderr %q", code, stderr)
	}
}

func TestConfigureDesktopProxyFailureLeavesDesktopUntouched(t *testing.T) {
	root := desktopCLIEnv(t)
	desktopEnsure = func(context.Context) (string, error) { return "", errors.New("private diagnostic") }
	code, stdout, stderr := captureOutput(t, func() int {
		return cmdConfigure([]string{"desktop", "--profile", "anthropic"})
	})
	if code == 0 || stdout != "" || strings.Contains(stderr, "private diagnostic") {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("proxy failure wrote Desktop files: %v, %v", entries, err)
	}
}

func TestDesktopModelsFilterAndDisplayNames(t *testing.T) {
	old := desktopGatewayModels
	t.Cleanup(func() { desktopGatewayModels = old })
	desktopGatewayModels = func(context.Context, *providers.Provider) ([]providers.ModelInfo, error) {
		return []providers.ModelInfo{
			{ID: "claude-sonnet-4-6", Name: "Sonnet Display", Gateway: true},
			{ID: "gpt-5", Name: "not Claude", Gateway: true},
			{ID: "claude-sonnet-4-6", Name: "duplicate", Gateway: true},
		}, nil
	}
	p := &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks,
		Options: providers.Options{Wire: providers.WireAIGateway},
		BaseURL: "https://example.com/ai-gateway/anthropic", Protocol: providers.ProtocolAnthropic}
	reg := &providers.Registry{}
	got, err := desktopModelList(context.Background(), reg, p)
	if err != nil || len(got) != 1 || got[0].Name != "claude-sonnet-4-6" || got[0].LabelOverride != "Sonnet Display" {
		t.Fatalf("models = %+v, %v", got, err)
	}
	desktopGatewayModels = func(context.Context, *providers.Provider) ([]providers.ModelInfo, error) {
		return nil, errors.New("upstream error with secret")
	}
	p.Models = map[string]string{"sonnet": "claude-sonnet-4-6[1m]", "opus": "claude-sonnet-4-6", "haiku": "gpt-5"}
	_, _, stderr := captureOutput(t, func() int {
		got, err = desktopModelList(context.Background(), reg, p)
		return 0
	})
	if err != nil || len(got) != 1 || got[0].Name != "claude-sonnet-4-6" || got[0].LabelOverride == "claude-sonnet-4-6" {
		t.Fatalf("fallback models = %+v, %v", got, err)
	}
	if strings.Contains(stderr, "secret") {
		t.Fatal("catalog warning leaked upstream error")
	}
}

func TestDesktopModelsAllResolveOnProfileRoute(t *testing.T) {
	old := desktopGatewayModels
	t.Cleanup(func() { desktopGatewayModels = old })
	desktopGatewayModels = func(context.Context, *providers.Provider) ([]providers.ModelInfo, error) {
		return []providers.ModelInfo{{ID: "claude-sonnet-4-6", Name: "S"}, {ID: "other:claude-opus-4-6", Name: "O"}}, nil
	}
	dev := &providers.Provider{Name: "dev", Preset: providers.PresetDatabricks, Options: providers.Options{Wire: providers.WireAIGateway},
		BaseURL: "https://example.com/ai-gateway/anthropic", Protocol: providers.ProtocolAnthropic}
	other := &providers.Provider{Name: "other"}
	reg := &providers.Registry{Providers: map[string]*providers.Provider{"dev": dev, "other": other}}
	got, err := desktopModelList(context.Background(), reg, dev)
	if err != nil || len(got) != 1 || got[0].Name != "claude-sonnet-4-6" {
		t.Fatalf("models = %+v, %v", got, err)
	}
	for _, m := range got {
		if tgt := providers.ParseTarget(reg, m.Name, dev); tgt.Provider.Name != "dev" {
			t.Fatalf("%s routes to %s", m.Name, tgt.Provider.Name)
		}
	}
}
