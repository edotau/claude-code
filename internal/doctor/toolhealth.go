package doctor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/tools"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/rtkwrap"
	"github.com/edotau/claude-code/internal/settings"
)

// toolDoctorOK is false only for a bin/<agent> entry that exists but cannot run (broken link, retired shim).
func toolDoctorOK(s tools.Status) bool {
	bin, ok := paths.AgentName(s.Name)
	if !ok {
		return true
	}
	if _, err := os.Lstat(filepath.Join(paths.BinDir(), bin)); os.IsNotExist(err) {
		return true
	}
	_, err := paths.AgentBin(bin)
	return err == nil
}

// agentVersionDetail renders one agent's version row: ok <ver>, warn <ver> <problem>, or not installed.
func agentVersionDetail(spec agent.Schema) (pass bool, detail string) {
	v, err := tools.Version(spec.Name)
	switch {
	case errors.Is(err, tools.ErrAgentNotInstalled):
		return true, "not installed"
	case err != nil:
		return true, "version unknown (" + err.Error() + ")"
	}
	if p := tools.VersionProblem(spec.CLI, v); p != "" {
		return false, "warn " + v + " " + p
	}
	return true, "ok " + v
}

// helpProbeTimeout bounds one `<cli> --help`; the probes run concurrently, so doctor waits for the slowest.
var helpProbeTimeout = 3 * time.Second

// agentHelp is the installed CLI's version and its help text (spec.CLI.HelpArgv); tests swap it.
var agentHelp = func(ctx context.Context, spec agent.Schema) (version, help string, err error) {
	if version, err = tools.Version(spec.Name); err != nil {
		return "", "", err
	}
	path, err := paths.AgentBin(spec.Name)
	if err != nil {
		if path, err = paths.LookPathReal(agentid.Binary(spec.Name)); err != nil {
			return "", "", fmt.Errorf("%w: %v", tools.ErrAgentNotInstalled, err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, helpProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, append(append([]string(nil), spec.CLI.HelpArgv...), helpFlagIfBare(spec)...)...)
	cmd.Env = agent.EnvSpec{Mode: agent.EnvAgent}.Environ(agent.ProbeContext(ctx, "")) // a help probe gets no credential
	if spec.Name == "pi" {
		cmd.Env = append(cmd.Env, tools.PiNodePathEnv(ctx)...) // the launcher's Node, else an older PATH node crashes
	}
	cmd.WaitDelay = 200 * time.Millisecond
	out, runErr := cmd.CombinedOutput() // some CLIs print help on stderr or exit non-zero
	text, err := helpText(out, runErr)
	return version, text, err
}

// helpText is a probe's output when it reads as help, else an error; a non-zero exit alone does not refuse it.
func helpText(out []byte, runErr error) (string, error) {
	usable := strings.Contains(strings.ToLower(string(out)), "usage") || helpFlagRe.Match(out) // "usage" or a --flag
	if len(out) == 0 || (runErr != nil && !usable) {
		return "", fmt.Errorf("no help output: %v", runErr)
	}
	return string(out), nil
}

var helpFlagRe = regexp.MustCompile(`--[a-z]`)

// helpFlagIfBare is --help when the row names no help subcommand.
func helpFlagIfBare(spec agent.Schema) []string {
	if len(spec.CLI.HelpArgv) == 0 {
		return []string{"--help"}
	}
	return nil
}

// flagMentioned reports whether help names flag as a whole token (--sandbox is not --sandbox-mode).
func flagMentioned(help, flag string) bool {
	return regexp.MustCompile(`(?:^|[^\w-])` + regexp.QuoteMeta(flag) + `(?:[^\w-]|$)`).MatchString(help)
}

// agentFlagDetail is the flag-drift row: ok, drift: <missing flags> (installed <v>), or skipped: <why>.
func agentFlagDetail(ctx context.Context, spec agent.Schema) (pass bool, detail string) {
	version, help, err := agentHelp(ctx, spec)
	switch {
	case errors.Is(err, tools.ErrAgentNotInstalled):
		return true, "skipped: not installed"
	case err != nil:
		return true, "skipped: help unavailable"
	}
	var missing []string
	for _, f := range spec.CLI.Flags {
		if !flagMentioned(help, f) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return false, "drift: " + strings.Join(missing, ", ") + " (installed " + version + ")"
	}
	return true, "ok"
}

// startFlagProbes runs every flag-declaring agent's help probe at once; the result reports their rows when called.
func startFlagProbes(ctx context.Context) func(check func(bool, bool, string, string)) {
	var specs []agent.Schema
	for _, spec := range agent.Roster() {
		if len(spec.CLI.Flags) > 0 {
			specs = append(specs, spec)
		}
	}
	pass, detail := make([]bool, len(specs)), make([]string, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Go(func() { pass[i], detail[i] = agentFlagDetail(ctx, spec) })
	}
	return func(check func(bool, bool, string, string)) {
		wg.Wait()
		for i, spec := range specs {
			check(pass[i], false, "agent flags "+spec.Name, detail[i])
		}
	}
}

// toolDoctorDetail renders one tool's doctor line: an agent's bin/ entry, else path + version, then extra PATH copies.
func toolDoctorDetail(ctx context.Context, s tools.Status) string {
	if bin, ok := paths.AgentName(s.Name); ok {
		return agentDoctorDetail(ctx, s, bin)
	}
	if s.Path == "" {
		if s.Hint != "" {
			return "not found (" + s.Hint + ")"
		}
		return "not found (claude-code install --tools=" + s.Name + ")"
	}
	var detail strings.Builder
	detail.WriteString(s.Path)
	if s.Version != "" {
		detail.WriteString(" ")
		detail.WriteString(s.Version)
	}
	switch {
	case s.NPMGlobal:
		detail.WriteString(" (npm-global; claude-code install --tools retires this)")
	case s.LocalBin:
		detail.WriteString(" (~/.local/bin)")
	}
	detail.WriteString(extraPATHCopies(ctx, s.Name, s.Path))
	return detail.String()
}

// agentDoctorDetail renders BinDir/<bin>: `<dst> → <Target> <version>`, a direct install, or why it is unusable.
func agentDoctorDetail(ctx context.Context, s tools.Status, bin string) string {
	dst := filepath.Join(paths.BinDir(), bin)
	if _, err := paths.AgentBin(bin); err != nil {
		if s.Path != "" {
			return err.Error() + " · on PATH: " + s.Path
		}
		return err.Error()
	}
	var detail strings.Builder
	detail.WriteString(dst)
	if s.Target != "" {
		detail.WriteString(" → ")
		detail.WriteString(s.Target)
	}
	version := s.Version
	if s.Target == "" || s.Target != s.Path {
		version = toolPATHVersion(ctx, dst)
	}
	if version != "" {
		detail.WriteString(" ")
		detail.WriteString(version)
	}
	if s.Target == "" {
		detail.WriteString(" (direct install in bin/)")
	}
	if s.NPMGlobal && s.Target == s.Path {
		detail.WriteString(" (npm-global)")
	}
	detail.WriteString(extraPATHCopies(ctx, s.Name, dst))
	return detail.String()
}

// extraPATHCopies lists PATH copies of name that resolve somewhere other than primary, as " · also on PATH: …".
func extraPATHCopies(ctx context.Context, name, primary string) string {
	first, _ := filepath.Abs(paths.Canon(primary)) // a broken symlink compares by its own path
	var out strings.Builder
	for _, path := range toolPATHCopies(name) {
		if real, _ := filepath.Abs(paths.Canon(path)); real == first {
			continue
		}
		extra := path
		if version := toolPATHVersion(ctx, path); version != "" {
			extra += " " + version
		}
		out.WriteString(" · also on PATH: ")
		out.WriteString(extra)
	}
	return out.String()
}

// toolPATHCopies lists distinct executables named name on PATH, skipping BinDir and the harness binary.
func toolPATHCopies(name string) []string {
	binDir, _ := filepath.Abs(paths.BinDir())
	self, _ := paths.SelfBinary()
	seen := map[string]bool{}
	var copies []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if abs, _ := filepath.Abs(dir); abs == binDir {
			continue
		}
		candidate := filepath.Join(dir, name)
		real := paths.ExecutableRealPath(candidate)
		if real == "" || real == self || filepath.Base(real) == "claude-code" || seen[real] {
			continue
		}
		seen[real] = true
		copies = append(copies, candidate)
	}
	return copies
}

// pathCopyVersionTimeout bounds `--version` on a PATH copy, shorter than tools' probe so doctor stays quick.
var pathCopyVersionTimeout = 2 * time.Second

// toolPATHVersion is the first --version line of a PATH copy, cached by path+size+mtime like the tools version cache.
func toolPATHVersion(ctx context.Context, path string) string {
	return tools.ProbedVersionLine(path, func(p string) string {
		return tools.FirstVersionLine(ctx, p, pathCopyVersionTimeout)
	})
}

// depDoctorDetail renders a host tool as `<path> <version>`, or missing; optional ones lead "warn" (⚠), required fail.
func depDoctorDetail(s tools.DepStatus) (pass bool, detail string) {
	if s.Path != "" {
		return true, strings.TrimSpace(s.Path + " " + s.Version)
	}
	detail = "missing (" + s.Class + ": " + s.Use + "); install: " + s.Fix
	if !s.Required {
		detail = "warn " + detail
	}
	return false, detail
}

// rtkHookRows is one row per external agent whose hooks list rtk-rewrite, with the RTK_WRAP mode it runs under.
func rtkHookRows() []check {
	raw := strings.ToLower(strings.TrimSpace(cmp.Or(os.Getenv(rtkwrap.ModeEnv), providers.CustomEnv()[rtkwrap.ModeEnv])))
	mode := raw
	if mode != "on" && mode != "debug" {
		mode = "off"
	}
	detail := mode + " (RTK_WRAP=" + raw + ")"
	if paths.RTKBinary() == "" {
		detail += " - rtk binary not found, rewrite skipped"
	}
	var rows []check
	for _, a := range []string{"codex", "copilot", "gemini", "opencode"} {
		specs := slices.Concat(settings.SpecsFor(a, "native"), settings.SpecsFor(a, "plugin"))
		if slices.ContainsFunc(specs, func(s settings.EventSpec) bool { return s.Name == "rtk-rewrite" }) {
			rows = append(rows, check{Name: "rtk hook " + a, OK: true, Detail: detail})
		}
	}
	return rows
}
