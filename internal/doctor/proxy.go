package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxyteam"
	"github.com/edotau/claude-code/internal/vault"
)

// ProxyTeamsKey is the vault Overlay entry holding the team overlay document.
const ProxyTeamsKey = "proxy-teams"

// loadTeams loads the team config; an unreadable vault is a problem and skips Load so last-good stays intact.
func loadTeams() (proxyteam.Config, []proxyteam.Problem) {
	var overlay []byte
	err := vault.View(func(t *vault.Tx) error {
		if s, ok := t.Get(vault.Overlay, ProxyTeamsKey); ok {
			overlay = []byte(s)
		}
		return nil
	})
	if err != nil {
		def, ps := proxyteam.Parse(assets.ProxyTeams(), nil)
		return def, append(ps, proxyteam.Problem{Field: "overlay", Detail: "vault unreadable: " + err.Error()})
	}
	return proxyteam.Load(overlay)
}

// ProxyTeams loads the team config and describes it: which config is in force, then one line per team.
func ProxyTeams() (lines []string, problems []proxyteam.Problem) {
	cfg, problems := loadTeams()
	switch {
	case len(problems) == 0:
		lines = append(lines, "teams: merged config")
	default:
		lines = append(lines, "teams: fallback in force (last good config, else the embedded default)")
	}
	for _, t := range cfg.Teams {
		var parts []string
		for _, f := range t.Fields() {
			parts = append(parts, f.Name+"="+f.Value)
		}
		lines = append(lines, "team "+strings.Join(parts, " "))
	}
	return lines, problems
}

// proxyTeamsStatus summarizes the team config for the doctor row.
func proxyTeamsStatus() (detail string, bad bool) {
	cfg, problems := loadTeams()
	if len(problems) > 0 {
		parts := make([]string, len(problems))
		for i, p := range problems {
			parts[i] = p.String()
		}
		return strconv.Itoa(len(problems)) + " problems: " + strings.Join(parts, "; "), true
	}
	names := make([]string, len(cfg.Teams))
	for i, t := range cfg.Teams {
		names[i] = t.Name
	}
	return fmt.Sprintf("%d teams (%s)", len(names), strings.Join(names, ", ")), false
}

// proxyStatus is the daemon status plus the team summary.
func proxyStatus(ctx context.Context) (detail string, bad bool) {
	detail, bad = proxyDaemonStatus(ctx)
	td, tbad := proxyTeamsStatus()
	return detail + "; teams: " + td, bad || tbad
}

func proxyDaemonStatus(ctx context.Context) (detail string, bad bool) {
	st, err := proxy.ReadState()
	if errors.Is(err, os.ErrNotExist) {
		return "not running", false
	}
	if err != nil || st.Port == 0 {
		return "proxy.json unreadable (cannot check the running build)", true
	}
	h, ok := proxy.Holder(ctx, st.Port)
	if !ok {
		return "stale proxy.json (port " + strconv.Itoa(st.Port) + " not answering)", false
	}
	if proxy.Stale(st) {
		return fmt.Sprintf("pid %d runs an older binary (build %s); the status line replaces it once idle", h.PID,
			h.Build), true
	}
	return fmt.Sprintf("port %d pid %d, %d sessions", st.Port, h.PID, h.Sessions), false
}
