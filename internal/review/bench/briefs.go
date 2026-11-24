package bench

import (
	"fmt"
	"strings"

	"github.com/edotau/claude-code/internal/assets"
)

func validateBrief(kind string, values map[string]string) error {
	for key, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("bench brief %s: empty %s", kind, key)
		}
	}
	return nil
}

func renderFixBrief(in assets.BenchFixInput) (string, error) {
	if err := validateBrief("fix", map[string]string{
		"OwnedLine": in.OwnedLine, "Verify": in.Verify, "Benchmark": in.Benchmark, "Package": in.Package,
		"Workdir": in.Workdir, "Cause": in.Cause, "Numbers": in.Numbers, "BeforeFile": in.BeforeFile,
		"BenchCommand": in.BenchCommand, "IdentityCommand": in.IdentityCommand,
	}); err != nil {
		return "", err
	}
	return assets.BenchFixBrief(in)
}

func renderReviewBrief(in assets.BenchReviewInput) (string, error) {
	if emptyDiff(in.Diff) {
		in.Diff = assets.BenchNoDiff()
	}
	if err := validateBrief("review", map[string]string{
		"OwnedLine": in.OwnedLine, "Verify": in.Verify, "Benchmark": in.Benchmark, "Package": in.Package,
		"Workdir": in.Workdir, "Numbers": in.Numbers, "BeforeFile": in.BeforeFile,
		"AfterFile": in.AfterFile, "Diff": in.Diff, "IdentityCommand": in.IdentityCommand,
	}); err != nil {
		return "", err
	}
	return assets.BenchReviewBrief(in)
}

// emptyDiff is true for a blank diff, bare or inside the driver's ```diff fence.
func emptyDiff(s string) bool {
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "```diff")) == ""
}
