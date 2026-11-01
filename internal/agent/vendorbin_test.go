package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/paths/pathstest"
)

func TestPlainPlansResolveVendorBinNotPATH(t *testing.T) {
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			source := vendorPATHFixture(t, paths.Vendors[name], "")
			t.Setenv("PATH", filepath.Dir(source))
			bin := pathstest.FakeVendor(t, name, "")
			spec, ok := LookupSchema(name)
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
