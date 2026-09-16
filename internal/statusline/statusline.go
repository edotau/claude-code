// Package statusline renders the one-line status bar from Claude Code's statusline JSON.
package statusline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/transcript"
)

// Input is the subset of Claude Code's statusline payload we read.
type Input struct {
	CWD            string `json:"cwd"`
	TranscriptPath string `json:"transcript_path"`
	Model          struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
	Workspace struct {
		CurrentDir string `json:"current_dir"`
	} `json:"workspace"`
	ContextWindow *struct {
		UsedPercentage *float64 `json:"used_percentage"`
	} `json:"context_window"`
}

// Git is the branch and dirty-file count of a directory; ok=false outside a repo.
type Git func(dir string) (branch string, dirty int, ok bool)

const (
	reset   = "\x1b[0m"
	bold    = "\x1b[1m"
	dim     = "\x1b[2m"
	red     = "\x1b[31m"
	green   = "\x1b[32m"
	yellow  = "\x1b[33m"
	blue    = "\x1b[34m"
	magenta = "\x1b[35m"
	cyan    = "\x1b[36m"
	sep     = dim + " · " + reset
)

// Run reads the payload from r and writes one line to w.
func Run(r io.Reader, w io.Writer) int {
	var in Input
	_ = json.NewDecoder(r).Decode(&in)
	fmt.Fprintln(w, Render(in, SessionProvider(), gitStatus))
	return 0
}

// Render builds the line: model · provider · context % · branch +dirty · cwd.
func Render(in Input, provider string, git Git) string {
	dir := in.Workspace.CurrentDir
	if dir == "" {
		dir = in.CWD
	}
	model := in.Model.DisplayName
	if model == "" {
		model = in.Model.ID
	}
	var parts []string
	if model != "" {
		parts = append(parts, bold+cyan+model+reset)
	}
	if provider != "" {
		parts = append(parts, magenta+provider+reset)
	}
	if pct, ok := contextPct(in); ok {
		c := green
		switch {
		case pct >= 85:
			c = red
		case pct >= 60:
			c = yellow
		}
		parts = append(parts, fmt.Sprintf("%s%d%% ctx%s", c, pct, reset))
	}
	if dir != "" && git != nil {
		if branch, dirty, ok := git(dir); ok {
			s := blue + branch + reset
			if dirty > 0 {
				s += fmt.Sprintf(" %s+%d%s", yellow, dirty, reset)
			}
			parts = append(parts, s)
		}
	}
	if dir != "" {
		parts = append(parts, dim+filepath.Base(dir)+reset)
	}
	return strings.Join(parts, sep)
}

// contextPct prefers Claude Code's own figure, else the transcript's last usage over the model window.
func contextPct(in Input) (int, bool) {
	if in.ContextWindow != nil && in.ContextWindow.UsedPercentage != nil {
		return int(*in.ContextWindow.UsedPercentage + 0.5), true
	}
	u, ok := transcript.LastUsage(in.TranscriptPath)
	if !ok {
		return 0, false
	}
	model := in.Model.ID
	if model == "" {
		model = u.Model
	}
	return u.Tokens * 100 / transcript.Window(model, u.Tokens), true
}

// SessionProvider names the upstream: a router path /p/<name>, else HARNESS_PROVIDER, else the base URL host.
func SessionProvider() string {
	base := os.Getenv("ANTHROPIC_BASE_URL")
	if u, err := url.Parse(base); err == nil && base != "" {
		if name, ok := strings.CutPrefix(u.Path, "/p/"); ok {
			return "router→" + strings.Trim(name, "/")
		}
	}
	if p := providers.Pin(providers.PinProvider); p != "" {
		return p
	}
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	return ""
}

// gitStatus runs one `git status --porcelain --branch` under a short timeout.
func gitStatus(dir string) (string, int, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "--no-optional-locks", "status", "--porcelain", "--branch").Output()
	if err != nil {
		return "", 0, false
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	branch := strings.TrimPrefix(lines[0], "## ")
	branch, _, _ = strings.Cut(branch, "...")
	branch = strings.TrimPrefix(branch, "No commits yet on ")
	return branch, len(lines) - 1, true
}
