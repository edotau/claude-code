// Package statusline renders Claude Code's status line from its statusline JSON in one of five styles
// (gradient, powerline, capsule, minimal, dashboard). It never errors: a blank payload still renders.
package statusline

import (
	"context"
	"encoding/json"
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

	"github.com/edotau/claude-code/internal/ansi"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/transcript"
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
}

// values holds the resolved per-segment strings the renderers paint.
type values struct {
	userHost   string
	path       string
	model      string
	provider   string
	router     string
	branch     string
	tokens     string
	duration   string
	session    string
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
	case "router":
		return v.router
	case "branch":
		return v.branch
	case "tokens":
		return v.tokens
	case "duration":
		return v.duration
	case "session":
		return v.session
	}
	return ""
}

// Run is `claude-code statusline [--style S]`: payload on r, rendered line(s) on w.
func Run(args []string, r io.Reader, w io.Writer) int {
	fs := flag.NewFlagSet("statusline", flag.ContinueOnError)
	style := fs.String("style", "", "gradient|powerline|capsule|minimal|dashboard (default: statusline/config.json, else gradient)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	raw, _ := io.ReadAll(r)
	fmt.Fprintln(w, Render(raw, *style))
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
	return render(collect(in, cfg), cfg, loadPalettes())
}

func render(v values, cfg Config, pal Palettes) string {
	stops := resolveGradient(cfg, pal)
	mode := colorMode(cfg)
	width := lineWidth(cfg)
	switch cfg.Style {
	case "dashboard":
		return clip(renderDashboard(v, cfg, pal, stops, mode, width), width)
	case "powerline", "capsule", "minimal":
		head := promptHead(v, cfg, pal, stops, mode)
		body := renderSegmented(v, cfg, pal, mode)
		if head != "" && body != "" {
			head += ansi.Paint(mode, ansi.Hex(pal.sem("dim")), sepPath, false)
		}
		return clip(head+body, width)
	}
	return clip(renderGradient(v, cfg, pal, stops, mode), width)
}

// resolveGradient returns the configured ramp's stops, else the embedded gemini ramp.
func resolveGradient(cfg Config, pal Palettes) []ansi.RGB {
	stops := pal.Gradients[cfg.Gradient]
	if len(stops) == 0 {
		stops = embeddedPalettes().Gradients["gemini"]
	}
	out := make([]ansi.RGB, len(stops))
	for i, h := range stops {
		out[i] = ansi.Hex(h)
	}
	return out
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
	provider, routed := sessionProvider()
	v := values{userHost: promptUser(), path: shortenDir(cwd)}
	if cfg.seg("provider", true) {
		v.provider = provider
	}
	if cfg.seg("model", true) {
		v.model = modelSegment(in, provider)
	}
	if cfg.seg("router", true) {
		v.router = routerSegment(routed)
	}
	if cfg.seg("git", true) {
		v.branch = gitBranch(cwd)
	}
	if cfg.seg("context", true) {
		v.contextPct = contextPct(in)
	}
	if cw := in.ContextWindow; cw != nil {
		v.tokens = approxTokens(cw.TotalInput + cw.TotalOutput)
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

// sessionProvider: a loopback router base /p/<name> (a routed launch), else HARNESS_PROVIDER (env, then
// pin file), else the registry default.
func sessionProvider() (name string, routed bool) {
	if u, err := url.Parse(os.Getenv("ANTHROPIC_BASE_URL")); err == nil {
		if rest, ok := strings.CutPrefix(u.Path, "/p/"); ok && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost") {
			name, _, _ = strings.Cut(rest, "/")
			return name, true
		}
	}
	if p := providers.Pin(providers.PinProvider); p != "" {
		return p, false
	}
	if reg := registry(); reg != nil {
		return reg.Default, false
	}
	return "", false
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
			return name + " ⇄" + prov
		}
	}
	if name == "" {
		name = in.Model.ID
	}
	return name
}

// routerTTL caps health probes at one per 5s across renders.
const routerTTL = 5 * time.Second

// routerSegment shows "router:<port>" while the daemon answers, "router:down" when this session needs it.
func routerSegment(routed bool) string {
	st, err := router.ReadState()
	if err == nil && st.Port > 0 && routerUp(st) {
		return "router:" + strconv.Itoa(st.Port)
	}
	if routed {
		return "router:down"
	}
	return ""
}

func routerUp(st router.State) bool {
	stamp := stampFile("router", paths.StateDir()+"\x00"+strconv.Itoa(st.PID)+"\x00"+strconv.Itoa(st.Port))
	if f := freshStamp(stamp, routerTTL, 1); f != nil {
		return f[0] == "up"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	state := "down"
	if router.Health(ctx, st.Port) == nil {
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

// approxTokens renders a session token total as "~12k tok" / "~1.2M tok"; zero hides the segment.
func approxTokens(n int) string {
	switch {
	case n <= 0:
		return ""
	case n < 1_000:
		return fmt.Sprintf("~%d tok", n)
	case n < 1_000_000:
		return fmt.Sprintf("~%dk tok", (n+500)/1_000)
	default:
		return fmt.Sprintf("~%.1fM tok", float64(n)/1_000_000)
	}
}
