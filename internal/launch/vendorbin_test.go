package launch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/vendors"
)

func TestPlainPlansResolveVendorBinNotPATH(t *testing.T) {
	for _, name := range vendors.Names() {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			source := vendorPATHFixture(t, paths.Vendors[name], "")
			t.Setenv("PATH", filepath.Dir(source))
			bin := pathstest.FakeVendor(t, name, "")
			spec, ok := vendors.Lookup(name)
			if !ok {
				t.Fatalf("missing vendor %s", name)
			}
			plan, err := plainPlan(spec, nil)
			if err != nil || plan.Binary != bin {
				t.Fatalf("plainPlan = %q, %v; want %q", plan.Binary, err, bin)
			}
			if err := os.Remove(bin); err != nil {
				t.Fatal(err)
			}
			_, err = plainPlan(spec, nil)
			if err == nil || !strings.Contains(err.Error(), "not installed at "+bin) {
				t.Fatalf("missing vendor: %v; want bin-dir error despite PATH vendor", err)
			}
		})
	}
}

func TestRoutedVendorPlanResolvesVendorBin(t *testing.T) {
	hermetic(t, "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "test-openai-key")
	source := vendorPATHFixture(t, "codex", "")
	t.Setenv("PATH", filepath.Dir(source))
	bin := pathstest.FakeVendor(t, "codex", "")
	plan, err := Vendor(context.Background(), "codex", Options{Provider: "openai", Direct: true}, []string{"--version"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Binary != bin {
		t.Errorf("Binary = %q, want %q", plan.Binary, bin)
	}
	if plan.Set[vendors.CodexKeyEnv] != "test-openai-key" {
		t.Errorf("routed credential = %q", plan.Set[vendors.CodexKeyEnv])
	}
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	_, err = Vendor(context.Background(), "codex", Options{Provider: "openai", Direct: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "not installed at "+bin) {
		t.Fatalf("missing routed vendor: %v; want bin-dir error despite PATH vendor", err)
	}
}
