package memory

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Access-driven nomination thresholds, computed from real recall data.
const (
	hotWindowDays = 30 // recall window for the hot check
	hotMinDays    = 3  // distinct in-window days that make an entry "hot"
	coldDays      = 90 // no recall for this long → archive candidate (log-age gated)
)

// AccessNominations turns access logs across every repo into doctor advisories: hot entries
// (recalled often — want a pointer in the injected bank) and cold ones (archive candidates, gated
// on log age — a young log proves nothing).
func AccessNominations(now time.Time) []string {
	files, err := filepath.Glob(filepath.Join(AccessStateDir(), "*.log"))
	if err != nil || len(files) == 0 {
		return nil
	}
	var noms []string
	for _, f := range files {
		key := strings.TrimSuffix(filepath.Base(f), ".log")
		stats, oldest := accessStatsAt(f, now, hotWindowDays)
		if oldest.IsZero() {
			continue
		}
		logCoversCold := oldest.Before(now.AddDate(0, 0, -coldDays))
		for entry, s := range stats {
			if s.Days >= hotMinDays {
				noms = append(noms, fmt.Sprintf(
					"%s/%s: hot — recalled on %d days in the last %dd; consider a pointer in the injected bank",
					key, entry, s.Days, hotWindowDays))
				continue
			}
			if logCoversCold && s.Newest.Before(now.AddDate(0, 0, -coldDays)) {
				noms = append(noms, fmt.Sprintf(
					"%s/%s: not recalled in %dd — archive candidate", key, entry, coldDays))
			}
		}
	}
	sort.Strings(noms) // map iteration order is random; doctor output must be stable
	return noms
}
