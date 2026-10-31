// Package statusline renders Claude Code's status line from its statusline JSON in one of five styles
// (gradient, powerline, capsule, minimal, dashboard). It never errors: a blank payload still renders.
package statusline

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/transcript"
	"github.com/edotau/claude-code/internal/ui/ansi"
	"github.com/edotau/claude-code/internal/ui/theme"
	"github.com/edotau/claude-code/internal/usage/usageledger"
)

// Input is the subset of Claude Code's statusline payload the renderer reads.
type Input struct {
	SessionID      string `json:"session_id"`
	CWD            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
	Exceeds200k    bool   `json:"exceeds_200k_tokens"`
	Model          struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
	Workspace struct {
		CurrentDir string `json:"current_dir"`
	} `json:"workspace"`
	Cost struct {
		DurationMS   int64 `json:"total_duration_ms"`
		LinesAdded   int   `json:"total_lines_added"`
		LinesRemoved int   `json:"total_lines_removed"`
	} `json:"cost"`
	ContextWindow *struct {
		Size           int      `json:"context_window_size"`
		TotalInput     int      `json:"total_input_tokens"`
		TotalOutput    int      `json:"total_output_tokens"`
		UsedPercentage *float64 `json:"used_percentage"`
	} `json:"context_window"`
	RateLimits *struct {
		FiveHour *rateWindow `json:"five_hour"`
		SevenDay *rateWindow `json:"seven_day"`
	} `json:"rate_limits"`
}

// rateWindow is one rate-limit window from the statusline payload's rate_limits object.
type rateWindow struct {
	UsedPercentage *float64 `json:"used_percentage"`
	ResetsAt       int64    `json:"resets_at"`
}

// values holds the resolved per-segment strings the renderers paint.
type values struct {
	userHost   string
	path       string
	model      string
	provider   string
	proxy      string
	branch     string
	total      string
	duration   string
	session    string
	agents     string
	limits     string
	contextPct int
	adds       int
	dels       int
}

// text returns a plain segment's value; context, git and path are drawn by each style itself.
func (v values) text(key string) string {
	switch key {
	case "model":
		return v.model
	case "provider":
		return v.provider
	case "proxy":
		return v.proxy
	case "branch":
		return v.branch
	case "total":
		return v.total
	case "duration":
		return v.duration
	case "session":
		return v.session
	case "agents":
		return v.agents
	case "limits":
		return v.limits
	}
	return ""
}

// Run is `claude-code theme statusline [--style S]`: payload on r, rendered line(s) on w. The caller supplies fs
// with its themed Usage (helptext imports this package, so the theming can't live here).
func Run(fs *flag.FlagSet, args []string, r io.Reader, w io.Writer) int {
	style := fs.String("style", "", "gradient|powerline|capsule|minimal|dashboard (default: config.json)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	raw, err := io.ReadAll(r)
	fmt.Fprintln(w, Render(raw, *style)) // best-effort: render whatever was read even on a short read
	if err != nil {
		return 1
	}
	return 0
}

// Render styles a statusline payload; style "" keeps the configured one.
func Render(raw []byte, style string) string {
	var in Input
	_ = json.Unmarshal(raw, &in)
	cfg := loadConfig()
	if style != "" {
		cfg.Style = style
	}
	return render(collect(in, cfg), cfg, theme.LoadPalettes())
}

func render(v values, cfg Config, pal theme.Palettes) string {
	pal = cfg.LiftPalette(pal)
	stops := cfg.Stops(pal)
	mode := cfg.Mode()
	width := lineWidth(cfg)
	switch cfg.Style {
	case "dashboard":
		return clip(renderDashboard(v, cfg, pal, stops, mode, width), width)
	case "powerline", "capsule", "minimal":
		head := promptHead(v, cfg, pal, stops, mode)
		body := renderSegmented(v, cfg, pal, mode)
		if head != "" && body != "" {
			head += ansi.Paint(mode, ansi.Hex(pal.Sem("dim")), sepPath, false)
		}
		return clip(head+body, width)
	}
	return clip(renderGradient(v, cfg, pal, stops, mode), width)
}

// collect resolves every enabled segment; disabled ones skip their IO.
func collect(in Input, cfg Config) values {
	cwd := in.Workspace.CurrentDir
	if cwd == "" {
		cwd = in.CWD
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	root, worktree := checkoutOf(cwd) // the path names the repo, so it holds still across worktrees and subdirs
	v := values{userHost: promptUser(), path: collapsePath(shortenDir(root), root, cfg.PathMax)}
	var provider string
	var routed bool
	if cfg.seg("provider", true) || cfg.seg("model", true) || cfg.seg("proxy", true) || cfg.seg("limits", true) {
		provider, routed = sessionProvider()
	}
	if cfg.seg("provider", true) {
		v.provider = providerLabel(cmp.Or(provider, modelFamily(in.Model.ID)))
	}
	if cfg.seg("model", true) {
		v.model = modelSegment(in, provider)
	}
	if cfg.seg("proxy", true) {
		v.proxy = proxySegment(routed)
	}
	if cfg.seg("git", true) && worktree != "" {
		v.branch = filepath.Base(worktree) // a linked worktree shows its own name, the main checkout its branch
	} else if cfg.seg("git", true) {
		v.branch = gitBranch(cwd)
	}
	if cfg.seg("agents", true) {
		v.agents = runningSegment(in)
	}
	if cfg.seg("limits", true) {
		v.limits = limitsSegment(in, provider)
	}
	if cfg.seg("context", true) {
		v.contextPct = contextPct(in)
	}
	if cfg.seg("total", true) {
		v.total = totalSegment(in)
	}
	v.duration = formatDuration(in.Cost.DurationMS)
	if id := in.SessionID; id != "" {
		v.session = id[:min(8, len(id))]
	}
	v.adds, v.dels = codeVelocity(in, cwd, cfg)
	return v
}

// registry is loaded at most once per render, and only when a segment needs it.
var registry = sync.OnceValue(func() *providers.Registry {
	reg, _ := providers.Load()
	return reg
})

// sessionProvider: a loopback proxy base /p/<name> (a routed launch), else the session profile (CLAUDE_PROFILE env, then
// the vault pin), else the registry default.
func sessionProvider() (name string, routed bool) {
	if u, err := url.Parse(os.Getenv("ANTHROPIC_BASE_URL")); err == nil {
		if rest, ok := strings.CutPrefix(u.Path, "/p/"); ok && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost") {
			name, _, _ = strings.Cut(rest, "/")
			return name, true
		}
	}
	if p := cmp.Or(providers.SessionProfile(), providers.Pin(providers.PinProfile)); p != "" {
		return p, false
	}
	if reg := registry(); reg != nil {
		return reg.Default, false
	}
	return "", false
}

// modelFamily guesses the family from a model id when no provider resolves (databricks-claude-… / claude-…).
func modelFamily(id string) string {
	switch id = models.Strip1M(strings.TrimSpace(id)); {
	case strings.HasPrefix(id, models.DatabricksPrefix):
		return "databricks"
	case models.FamilyKind(id) == models.FamilyClaude:
		return "anthropic"
	}
	return ""
}

// providerLabel renders "<provider>·<profile>" from the registry row, the provider alone when the profile bears its name.
func providerLabel(name string) string {
	if reg := registry(); reg != nil {
		if p, err := reg.Get(name); err == nil && p.Provider != "" {
			if p.Name == p.Provider {
				return p.Provider
			}
			return p.Provider + "·" + p.Name
		}
	}
	return name
}

// modelSegment is the friendly name; a cross-provider slot ("openai:gpt-5") renders "gpt-5 ⇄openai".
func modelSegment(in Input, session string) string {
	name := in.Model.DisplayName
	if i := strings.Index(name, " · "); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimSpace(strings.ReplaceAll(name, " (1M context)", " [1m]"))
	id := models.Strip1M(strings.TrimSpace(in.Model.ID))
	// Registry check: ollama tags like "qwen3:30b" carry a colon without naming a provider.
	if prov, model, ok := strings.Cut(id, ":"); ok && prov != session && model != "" {
		if reg := registry(); reg != nil && reg.Providers[prov] != nil {
			if name == "" || models.Strip1M(name) == id {
				name = model
			}
			return tag1M(in, name) + " ⇄" + prov
		}
	}
	if name == "" {
		name = in.Model.ID
	}
	return tag1M(in, name)
}

// tag1M marks a 1M session whose display name lacks Claude Code's "(1M context)" (custom ids, e.g. Databricks).
func tag1M(in Input, name string) string {
	oneM := models.Has1M(strings.TrimSpace(in.Model.ID)) ||
		(in.ContextWindow != nil && in.ContextWindow.Size >= models.ContextWindow1M)
	if !oneM || models.Has1M(name) {
		return name
	}
	return name + " [1m]"
}

// proxyTTL caps health probes at one per 5s across renders.
const proxyTTL = 5 * time.Second

// proxySegment shows "proxy:<port>" while the daemon answers, "proxy:down" when this session needs it.
func proxySegment(routed bool) string {
	st, err := proxy.ReadState()
	if err == nil && st.Port > 0 && proxyUp(st) {
		return "proxy:" + strconv.Itoa(st.Port)
	}
	if routed {
		return "proxy:down"
	}
	return ""
}

func proxyUp(st proxy.State) bool {
	stamp := stampFile("proxy", paths.StateDir()+"\x00"+strconv.Itoa(st.PID)+"\x00"+strconv.Itoa(st.Port))
	if f := freshStamp(stamp, proxyTTL, 1); f != nil {
		return f[0] == "up"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	state := "down"
	if proxy.Health(ctx, st.Port) == nil {
		state = "up"
	}
	writeTimedStamp(stamp, state)
	return state == "up"
}

// contextPct prefers Claude Code's used_percentage, else the transcript's last usage over the window.
func contextPct(in Input) int {
	if cw := in.ContextWindow; cw != nil && cw.UsedPercentage != nil {
		return clampPct(int(*cw.UsedPercentage + 0.5))
	}
	used, model := lastUsage(in.TranscriptPath)
	if used == 0 {
		return 0
	}
	if in.Model.ID != "" {
		model = in.Model.ID
	}
	return clampPct(used * 100 / contextLimit(in, model, used))
}

// contextLimit: a reported window is ground truth; exceeds_200k next; else infer from the model/env pins.
func contextLimit(in Input, model string, used int) int {
	if cw := in.ContextWindow; cw != nil && cw.Size > 0 {
		return cw.Size
	}
	if in.Exceeds200k {
		return models.ContextWindow1M
	}
	return transcript.Window(model, used)
}

// lastUsage memos (size, mtime) → usage: the record only changes when the transcript grows.
func lastUsage(path string) (int, string) {
	if path == "" {
		return 0, ""
	}
	fi, statErr := os.Stat(path)
	stamp := stampFile("context", path)
	size, mtime := "", ""
	if statErr == nil {
		size, mtime = strconv.FormatInt(fi.Size(), 10), strconv.FormatInt(fi.ModTime().UnixNano(), 10)
		if f := readStamp(stamp); len(f) == 4 && f[0] == size && f[1] == mtime {
			if n, err := strconv.Atoi(f[2]); err == nil {
				return n, f[3]
			}
		}
	}
	u, ok := transcript.LastUsage(path)
	if !ok {
		return 0, ""
	}
	if statErr == nil {
		writeStamp(stamp, size, mtime, strconv.Itoa(u.Tokens), u.Model)
	}
	return u.Tokens, u.Model
}

func clampPct(pct int) int { return max(0, min(100, pct)) }

// velocityTTL bounds the git diff + untracked sweep, the largest per-render cost.
const velocityTTL = 5 * time.Second

// codeVelocity is the uncommitted git diff (Claude Code's ledger misses Bash edits), else the ledger.
func codeVelocity(in Input, cwd string, cfg Config) (adds, dels int) {
	if !cfg.seg("velocity", true) {
		return 0, 0
	}
	if cfg.VelocityMode == "raw" {
		return in.Cost.LinesAdded, in.Cost.LinesRemoved
	}
	stamp := stampFile("velocity", cwd+"\x00"+in.TranscriptPath)
	if f := freshStamp(stamp, velocityTTL, 2); f != nil {
		a, aerr := strconv.Atoi(f[0])
		d, derr := strconv.Atoi(f[1])
		if aerr == nil && derr == nil {
			return a, d
		}
	}
	adds, dels = in.Cost.LinesAdded, in.Cost.LinesRemoved
	if a, d, ok := treeDiffStat(cwd); ok {
		adds, dels = a, d
	}
	writeTimedStamp(stamp, strconv.Itoa(adds), strconv.Itoa(dels))
	return adds, dels
}

func formatDuration(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d <= 0:
		return ""
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// collapsePath drops short to its base name when it exceeds max runes (0 disables the cap).
func collapsePath(short, cwd string, max int) string {
	if max > 0 && utf8.RuneCountInString(short) > max {
		return filepath.Base(cwd)
	}
	return short
}

// shortenDir collapses $HOME to "~" (a sibling like /home/userx is left alone).
func shortenDir(p string) string {
	if p == "" {
		return "~"
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return p
}

// totalSegment is "total:<n>": this session's payload tokens plus its finished dispatches', "+" while a dispatch runs.
func totalSegment(in Input) string {
	n := 0
	if cw := in.ContextWindow; cw != nil {
		n = cw.TotalInput + cw.TotalOutput
	}
	li, lo, _ := usageledger.TotalsBySession(in.SessionID)
	n += li + lo
	if n <= 0 {
		return ""
	}
	out := "total:" + sigTokens(n)
	if _, dispatches := runningCounts(in); dispatches > 0 {
		out += "+"
	}
	return out
}

// sigTokens renders n with 2-3 significant figures: 812, 1.23k, 45.6k, 812k, 1.23M, 12.3M.
func sigTokens(n int) string {
	if n < 1_000 {
		return strconv.Itoa(n)
	}
	f, unit := float64(n)/1_000, "k"
	if f >= 999.5 {
		f, unit = f/1_000, "M"
	}
	switch {
	case f >= 99.5:
		return fmt.Sprintf("%.0f%s", f, unit)
	case f >= 9.995:
		return fmt.Sprintf("%.1f%s", f, unit)
	}
	return fmt.Sprintf("%.2f%s", f, unit)
}
