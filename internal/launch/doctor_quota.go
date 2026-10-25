package launch

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/route"
)

// quotaDoctorRows is one row per exhausted or low account in route's quota.json, with its reset and the manual fix.
func quotaDoctorRows(now time.Time) []Check {
	st := route.LoadState()
	keys := make([]string, 0, len(st))
	for k := range st {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var rows []Check
	for _, k := range keys {
		e := st[k]
		s := e.Status(now)
		if s != route.StateExhausted && s != route.StateLow {
			continue
		}
		d := fmt.Sprintf("%s at %.0f%%", s, e.UsedPct)
		if u := e.Until(now); !u.IsZero() {
			d += ", resets " + u.Local().Format("2006-01-02 15:04")
		}
		rows = append(rows, Check{Name: "quota " + k, Detail: d + "; " + quotaFix(k)})
	}
	rows = append(rows, Check{Name: "quota gemini", OK: true,
		Detail: "failure-driven only (Antigravity's quota endpoint denies access)"})
	return rows
}

// quotaFix is the manual remedy; the router itself never changes accounts.
func quotaFix(key string) string {
	switch {
	case strings.HasPrefix(key, "copilot:"):
		return "routed around; `claude-code configure github` is the only way to change the Copilot account"
	case strings.HasPrefix(key, "opencode:"):
		return "routed around; `opencode auth login` changes opencode's login"
	}
	return "routed around until the reset; `claude-code route` shows the pick"
}
