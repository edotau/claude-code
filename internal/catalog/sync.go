package catalog

import (
	"fmt"
	"time"

	"github.com/edotau/claude-code/internal/databricks/serving"
	"github.com/edotau/claude-code/internal/providers"
)

// UsableIn is Usable over a discovery snapshot's model rows.
func UsableIn(p *providers.Provider, infos []providers.ModelInfo, dialects []string) []providers.ModelInfo {
	return Usable(p, byName(infos), dialects)
}

// Seal stores snapshot in the vault when it is new or freshly verified; sealed is false when skipped.
func Seal(p *providers.Provider, snapshot serving.Snapshot, verified bool) (sealed bool, err error) {
	if snapshot.CacheHit && !verified {
		return false, nil
	}
	if err := serving.Save(p, snapshot); err != nil {
		return false, err
	}
	return true, nil
}

// SyncSummaryLines are the discovery and verification lines that head a sync report.
func SyncSummaryLines(snapshot serving.Snapshot, verified bool) []string {
	source := "live discovery"
	if snapshot.CacheHit {
		source = "encrypted cache hit"
	}
	lines := []string{fmt.Sprintf("Valid identity (not endpoint permissions); %s, discovered %s",
		source, snapshot.DiscoveredAt.Format(time.RFC3339))}
	label := "historical probes"
	if verified {
		label = "fresh probes"
	}
	ok, failed, tools := 0, 0, 0
	for _, c := range snapshot.Checks {
		if c.Status == "ok" {
			ok++
		} else {
			failed++
		}
		if c.Tools == "yes" {
			tools++
		}
	}
	if snapshot.CheckedAt.IsZero() {
		return append(lines, "Verification: listing only; models are discovered/unverified")
	}
	return append(lines,
		fmt.Sprintf("Verification (%s, %s): %d HTTP accepted, %d failed, %d tool definitions accepted",
			label, snapshot.CheckedAt.Format(time.RFC3339), ok, failed, tools),
		"Probes test HTTP acceptance, not generated answers or actual tool calls; untested models remain unverified.")
}
