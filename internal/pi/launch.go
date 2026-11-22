package pi

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
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

// CodemodeModes are the --codemode values: Pi's codemode.mode setting.
var CodemodeModes = []string{"on", "only"}

// CodemodeEnv is the vault or process setting (off|on|only) that turns code mode on for every pi --tools dispatch.
const CodemodeEnv = "PI_CODEMODE"

// ResolveCodemode is the typed --codemode, else the PI_CODEMODE setting under --tools; any other setting value is off.
func ResolveCodemode(typed string, tools bool) (string, error) {
	if typed != "" || !tools {
		return typed, nil
	}
	mode := strings.ToLower(strings.TrimSpace(cmp.Or(os.Getenv(CodemodeEnv), providers.CustomEnv()[CodemodeEnv])))
	if !slices.Contains(CodemodeModes, mode) {
		return "", nil
	}
	return mode, nil
}

// Experiment is the trial a run of req carries, from the same RTK_WRAP sources Wire reads.
func (piEngine) Experiment(req agent.RunOptions) string {
	return experiment(req.Codemode, rtkMode())
}

func rtkMode() string {
	return cmp.Or(os.Getenv(rtkwrap.ModeEnv), providers.CustomEnv()[rtkwrap.ModeEnv])
}

// experiment is the one trial a run carries; code mode wins over RTK_WRAP=on, whose shim never wraps under it.
func experiment(codemode, rtk string) string {
	switch {
	case codemode != "":
		return "codemode"
	case strings.EqualFold(strings.TrimSpace(rtk), "on"):
		return "rtk-wrap"
	}
	return "none"
}

// codemodeLaunchEnv is env exporting the effective code mode to Pi and putting the shim dir on PATH for it.
func codemodeLaunchEnv(env []string, mode string) []string {
	return rtkwrap.Launch(env, rtkMode(), mode, agent.NoticeOut)
}

// lastEnv is the value the child gets for name: the last entry of env.
func lastEnv(env []string, name string) string {
	v := ""
	for _, kv := range env {
		if s, ok := strings.CutPrefix(kv, name+"="); ok {
			v = s
		}
	}
	return v
}

// checkCodemode refuses a code mode Pi would not run as asked: an unknown mode or no tools.
func checkCodemode(req agent.RunOptions, _ []string) error {
	if req.Codemode == "" {
		return nil
	}
	if !slices.Contains(CodemodeModes, req.Codemode) {
		return fmt.Errorf("pi: --codemode %q is not one of %s", req.Codemode, strings.Join(CodemodeModes, "|"))
	}
	if !req.Tools {
		return errors.New("pi: --codemode needs --tools: its scripts call the guarded tools")
	}
	return nil
}

// codemodeArgv loads the builtin codemode extension explicitly (--no-extensions drops it) and turns its tool on.
func codemodeArgv(mode string) []string {
	if mode == "" {
		return nil
	}
	return []string{"-e", codemodeSource, "--tools", "+" + codemodeTool}
}

// writeCodemodeSettings sets codemode.mode in the run's private settings.json, the file WriteConfig wrote.
func writeCodemodeSettings(env []string, mode string) error {
	if mode == "" {
		return nil
	}
	file := filepath.Join(lastEnv(env, agentDirEnv), "settings.json")
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
