package memory

import (
	"fmt"
	"sort"
	"time"
)

// Access-driven nomination thresholds, computed from real recall data.
const (
	hotWindowDays = 30 // recall window for the hot check
	hotMinDays    = 3  // distinct in-window days that make an entry "hot"
	coldDays      = 90 // no recall for this long → archive candidate (log-age gated)
)

// AccessNominations turns the global bank's access log into doctor advisories: hot entries
// (recalled often — want a pointer in the injected bank) and cold ones (archive candidates, gated
// on log age — a young log proves nothing). Covers both bank files and native topics, whichever
// `LogAccess` recorded a hit for.
func AccessNominations(now time.Time) []string {
	stats, oldest := AccessStats(GlobalDir(), now, hotWindowDays)
	if oldest.IsZero() {
		return nil
	}
	logCoversCold := oldest.Before(now.AddDate(0, 0, -coldDays))
	var noms []string
	for entry, s := range stats {
		if s.Days >= hotMinDays {
			noms = append(noms, fmt.Sprintf(
				"%s: hot — recalled on %d days in the last %dd; consider a pointer in the injected bank",
				entry, s.Days, hotWindowDays))
			continue
		}
		if logCoversCold && s.Newest.Before(now.AddDate(0, 0, -coldDays)) {
			noms = append(noms, fmt.Sprintf(
				"%s: not recalled in %dd — archive candidate", entry, coldDays))
		}
	}
	sort.Strings(noms) // map iteration order is random; doctor output must be stable
	return noms
}
