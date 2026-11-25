package pi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/route"
	"github.com/edotau/claude-code/internal/rtkwrap"
)

// writeGuard is WriteGuard; tests swap it to prove a failed write refuses the run.
var writeGuard = WriteGuard

const (
	// codemodeTool is Pi's script tool; codemodeSource is the builtin extension the guard accepts it from.
	codemodeTool   = "codemode"
	codemodeSource = "builtin:codemode"
	// guardCodemodeEnv tells the guard to allow codemodeTool, and only when codemodeSource registered it.
	guardCodemodeEnv = "HARNESS_PI_GUARD_CODEMODE"
)

// ResolveCodemode is the typed --codemode (a typed off stays off), else the vault CODEMODE under --tools, else "".
func ResolveCodemode(typed string, tools bool) (string, error) {
	if typed != "" || !tools {
		return typed, nil
	}
	mode := strings.ToLower(strings.TrimSpace(providers.CustomEnv()[rtkwrap.CodemodeEnv]))
	if mode == route.CodemodeOff || !slices.Contains(route.Codemodes, mode) {
		return "", nil
	}
	return mode, nil
}

// codemodeOn reports whether a request's Codemode turns code mode on: "" and a typed "off" do not.
func codemodeOn(mode string) bool { return rtkwrap.CodemodeOn(mode) }

// Experiment is the trial a run of req carries: code mode, else rtk wrapping wherever its bash tool runs shimmed.
func (piEngine) Experiment(req agent.RunOptions) string {
	return experiment(req.Codemode, req.Tools)
}

// experiment is the one trial a run carries; the shims never wrap under code mode, nor on Windows (no shims).
func experiment(codemode string, bash bool) string {
	switch {
	case codemodeOn(codemode):
		return "codemode"
	case bash && runtime.GOOS != "windows":
		return "rtk-wrap"
	}
	return "none"
}

// codemodeLaunchEnv is env exporting the effective code mode to Pi and putting the shim dir on PATH for it.
func codemodeLaunchEnv(env []string, mode string) []string {
	return rtkwrap.Launch(env, mode, agent.NoticeOut)
}

// checkCodemode refuses a code mode Pi would not run as asked: an unknown mode or no tools.
func checkCodemode(req agent.RunOptions, _ []string) error {
	if req.Codemode == "" {
		return nil
	}
	if !slices.Contains(route.Codemodes, req.Codemode) {
		return fmt.Errorf("pi: --codemode %q is not one of %s", req.Codemode, strings.Join(route.Codemodes, "|"))
	}
	if !codemodeOn(req.Codemode) {
		return nil
	}
	if !req.Tools {
		return errors.New("pi: --codemode needs --tools: its scripts call the guarded tools")
	}
	return nil
}

// codemodeArgv loads the builtin codemode extension explicitly (--no-extensions drops it) and turns its tool on.
func codemodeArgv(mode string) []string {
	if !codemodeOn(mode) {
		return nil
	}
	return []string{"-e", codemodeSource, "--tools", "+" + codemodeTool}
}

// writeCodemodeSettings sets codemode.mode in the run's private settings.json, the file WriteConfig wrote.
func writeCodemodeSettings(env []string, mode string) error {
	if !codemodeOn(mode) {
		return nil
	}
	file := filepath.Join(proc.LastEnv(env, agentDirEnv), "settings.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("pi codemode: %w", err)
	}
	s := map[string]any{}
	if err = json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("pi codemode: %s: %w", file, err)
	}
	s["codemode"] = map[string]any{"mode": mode}
	if raw, err = json.MarshalIndent(s, "", " "); err != nil {
		return err
	}
	if _, err = paths.WriteIfChanged(file, raw, 0o600); err != nil {
		return fmt.Errorf("pi codemode: %w", err)
	}
	return nil
}
