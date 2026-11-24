package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	bolterrors "go.etcd.io/bbolt/errors"

	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/proxyteam"
	"github.com/edotau/claude-code/internal/vault"
)

// ProxyTeamsKey is the vault Overlay entry holding the team overlay document.
const ProxyTeamsKey = "proxy-teams"

const vaultBusyNote = "vault busy, last-good config in force"

// readOverlay reads the team overlay from the vault, retrying once (a busy vault is usually transient).
func readOverlay() (overlay []byte, err error) {
	for range 2 {
		overlay = nil
		err = vault.View(func(t *vault.Tx) error {
			if s, ok := t.Get(vault.Overlay, ProxyTeamsKey); ok {
				overlay = []byte(s)
			}
			return nil
		})
		if err == nil {
			break
		}
	}
	return overlay, err
}

// loadTeams skips Load on a vault error so last-good stays intact: a lock timeout is busy, any other error a problem.
func loadTeams() (cfg proxyteam.Config, problems []proxyteam.Problem, busy bool) {
	overlay, err := readOverlay()
	if errors.Is(err, bolterrors.ErrTimeout) {
		return proxyteam.Config{}, nil, true
	}
	if err != nil {
		return proxyteam.Config{}, []proxyteam.Problem{{Field: "overlay", Detail: "vault unreadable: " + err.Error()}}, false
	}
	cfg, problems = proxyteam.Load(overlay)
	return cfg, problems, false
}

// ProxyTeams loads the team config and describes it: which config is in force, then one line per team.
func ProxyTeams() (lines []string, problems []proxyteam.Problem) {
	cfg, problems, busy := loadTeams()
	switch {
	case busy:
		return []string{"teams: " + vaultBusyNote}, nil
	case len(problems) == 0:
		lines = append(lines, "teams: merged config")
	default:
		lines = append(lines, "teams: problems found, fallback config in force (last good, else the embedded default)")
	}
	for _, t := range cfg.Teams {
		var parts []string
		for _, f := range t.Fields() {
			parts = append(parts, f.Name+"="+f.Value)
		}
		lines = append(lines, "team "+strings.Join(parts, " "))
	}
	scopes, scopeProblems := scopeReport(context.Background(), cfg, false)
	return append(lines, scopes...), append(problems, scopeProblems...)
}

// probeWorkers bounds the concurrent /healthz probes of scopeReport.
const probeWorkers = 4

// scopeOverReserved: sc's id is at or past its detached team's Reserved, or the team is not a detached one (reserve 0).
func scopeOverReserved(sc proxy.Scope, cfg proxyteam.Config) (reserved int, over bool) {
	for _, t := range cfg.Teams {
		if t.Name == sc.Team && t.Host == proxyteam.HostDetached {
			n, err := strconv.Atoi(sc.ID)
			return t.Reserved, err != nil || n >= t.Reserved
		}
	}
	return 0, true
}

// scopeReport probes the live detached scopes concurrently and returns one line each plus a problem per flagged scope.
func scopeReport(ctx context.Context, cfg proxyteam.Config, busy bool) (lines []string, probs []proxyteam.Problem) {
	scopes := proxy.DetachedScopes()
	lines = make([]string, len(scopes))
	reasons := make([]string, len(scopes))
	var wg sync.WaitGroup
	sem := make(chan struct{}, probeWorkers)
	for i, sc := range scopes {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			h, ok := proxy.Holder(ctx, sc.Port)
			lines[i] = proxy.ScopeLine(sc, h.Inflight)
			var why []string
			if !ok {
				why = append(why, "not serving (draining or wedged)")
			}
			if n, bad := scopeOverReserved(sc, cfg); bad && !busy {
				why = append(why, fmt.Sprintf("over reserved=%d: unmanaged; gateway proxy --stop stops it", n))
			}
			reasons[i] = strings.Join(why, "; ")
			if reasons[i] != "" {
				lines[i] += " " + reasons[i]
			}
		}()
	}
	wg.Wait()
	for i, r := range reasons {
		if r != "" {
			probs = append(probs, proxyteam.Problem{Team: scopes[i].Team, Field: "scope " + scopes[i].ID, Detail: r})
		}
	}
	return lines, probs
}

// proxyTeamsStatus summarizes the loaded team config for the doctor row.
func proxyTeamsStatus(cfg proxyteam.Config, problems []proxyteam.Problem, busy bool) (detail string, bad bool) {
	if busy {
		return vaultBusyNote, false
	}
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
	cfg, problems, busy := loadTeams()
	td, tbad := proxyTeamsStatus(cfg, problems, busy)
	lines, scopeProblems := scopeReport(ctx, cfg, busy)
	if n := len(lines); n > 0 {
		td += fmt.Sprintf("; %d standing proxies live", n)
	}
	if over := len(scopeProblems); over > 0 {
		td += fmt.Sprintf("; %d need attention: %s", over, strings.Join(flaggedLines(lines), " | "))
		tbad = true
	}
	return detail + "; teams: " + td, bad || tbad
}

// flaggedLines keeps the lines scopeReport flagged: over reserved, or not serving.
func flaggedLines(lines []string) []string {
	return slices.DeleteFunc(slices.Clone(lines), func(l string) bool {
		return !strings.Contains(l, " over reserved=") && !strings.Contains(l, " not serving")
	})
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
