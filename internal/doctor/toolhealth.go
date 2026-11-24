package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/edotau/claude-code/internal/textutil"
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

// startFlagProbes starts every flag-declaring agent's help probe; the result reports them and the code-mode rows.
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
		for _, name := range agentid.CodeModeAgents() {
			if variable := agentid.Routing(name).CodeMode.Var; variable != "" {
				pass, detail := codeModeEnvDetail(name, variable)
				check(pass, false, "code mode env "+name, detail)
			}
		}
		pass, detail := leftoverCodemodeDetail(vaultEnv())
		check(pass, false, "vault env "+legacyCodemodeEnv, detail)
	}
}

// legacyCodemodeEnv is the pre-rename vault name of CODEMODE; nothing reads it.
const legacyCodemodeEnv = "PI_CODEMODE"

// vaultEnv is the vault env bucket the leftover row reads; tests swap it.
var vaultEnv = providers.CustomEnv

// leftoverCodemodeDetail warns when the vault still holds the pre-rename variable.
func leftoverCodemodeDetail(env map[string]string) (pass bool, detail string) {
	if _, ok := env[legacyCodemodeEnv]; !ok {
		return true, "not set"
	}
	return false, "warn unread: renamed to " + rtkwrap.CodemodeEnv + "; run `claude-code vault env unset " +
		legacyCodemodeEnv + " --yes`"
}

// codeModeEnvDetail is ok while the installed CLI still names its code-mode variable, else a drift warning.
func codeModeEnvDetail(name, variable string) (pass bool, detail string) {
	path, err := vendorBinary(name)
	if err != nil {
		return true, "skipped: not installed"
	}
	// The scan is cached per binary build, like the version probe; a read error caches nothing.
	found := tools.ProbedSaltedLine(path, "codemode-env "+variable, func(p string) string {
		ok, err := binaryMentions(p, variable)
		switch {
		case err != nil:
			return ""
		case ok:
			return "present"
		}
		return "absent"
	})
	switch found {
	case "":
		return true, "skipped: binary unreadable"
	case "present":
		return true, "ok"
	}
	detail = "warn drift: " + variable + " no longer in the installed CLI"
	if version, err := tools.Version(name); err == nil {
		detail += " (installed " + version + ")"
	}
	return false, detail
}

// vendorBinary is the executable a code-mode scan reads: the PATH copy outside BinDir, else the bin entry.
func vendorBinary(name string) (string, error) {
	path, err := paths.LookPathReal(agentid.Binary(name))
	if err != nil {
		if path, err = paths.AgentBin(name); err != nil {
			return "", err
		}
	}
	return filepath.EvalSymlinks(path)
}

// binaryMentions reports whether the file at path contains name, streamed in 1 MiB chunks (a CLI can be 380 MB).
func binaryMentions(path, name string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	needle := []byte(name)
	chunk := make([]byte, 1<<20)
	var tail []byte // the last len(name)-1 bytes read, so a name split across chunks still matches
	for {
		n, rerr := f.Read(chunk)
		window := append(tail, chunk[:n]...)
		if bytes.Contains(window, needle) {
			return true, nil
		}
		tail = window[max(0, len(window)-len(needle)+1):]
		if rerr == io.EOF {
			return false, nil
		}
		if rerr != nil {
			return false, rerr
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

// rtkMinVersion is the oldest rtk whose `rewrite --` and exit-code contract the hooks rely on.
const rtkMinVersion = "1.1.0"

// rtkVersionStatus shows the rtk binary's version and warns (never fails) below rtkMinVersion.
func rtkVersionStatus(ctx context.Context) (bool, string) {
	bin := paths.RTKBinary()
	if bin == "" {
		return true, "rtk not installed"
	}
	line := toolPATHVersion(ctx, bin)
	v := regexp.MustCompile(`\d+(\.\d+)+`).FindString(line)
	if v == "" {
		return true, "version unknown (" + bin + " --version)"
	}
	if c, ok := textutil.CompareVersions(v, rtkMinVersion); ok && c < 0 {
		return false, "warn rtk " + v + " is below " + rtkMinVersion + " — run make install"
	}
	return true, "rtk " + v
}

// rtkHookRows reports how rtk reaches each agent, including code mode and missing PATH binaries.
func rtkHookRows() []check {
	rows := make([]check, 0, 7)
	for _, a := range []string{"claude", "codex", "copilot", "gemini", "opencode", "pi"} {
		rows = append(rows, rtkAgentRow(a))
	}
	if _, err := exec.LookPath("rtk"); err != nil {
		rows = append(rows, check{Name: "rtk PATH", Detail: "warn rtk not on PATH (pinned binary may still work)"})
	}
	return rows
}

func rtkAgentRow(a string) check {
	row := check{Name: "rtk hook " + a, OK: true}
	switch {
	case rtkwrap.CodeMode():
		row.Detail = "off in code mode (CODEMODE)"
	case a == "gemini":
		row.Detail = "awareness text only"
	case paths.RTKBinary() == "":
		row.OK, row.Detail = false, "warn rtk binary not installed"
	case a == "copilot":
		row.Detail = "rewrite (SDK tool)"
	case a == "pi":
		row.Detail = "shims"
		if !piShimsInstalled() {
			row.OK, row.Detail = false, "warn shims not installed"
		}
	default:
		specs := slices.Concat(settings.SpecsFor(a, "native"), settings.SpecsFor(a, "plugin"))
		row.Detail = "rewrite (hook)"
		if !slices.ContainsFunc(specs, func(s settings.EventSpec) bool { return s.Name == "rtk-rewrite" }) || !rtkHookInstalled(a) {
			row.OK, row.Detail = false, "warn rtk-rewrite hook not installed"
		}
	}
	return row
}

func rtkHookInstalled(a string) bool {
	if a == "opencode" { // the plugin is generated and injected at launch, not installed in a global config
		return true
	}
	file := settings.SettingsFile()
	command := settings.DispatcherCommand(settings.EventPreToolUse)
	if a == "codex" {
		home, err := paths.CodexHome()
		if err != nil {
			return false
		}
		file = filepath.Join(home, "hooks.json")
		command = (settings.EventSpec{Event: settings.EventPreToolUse, Name: "rtk-rewrite"}).Command()
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	var config struct {
		Hooks map[string][]settings.MatcherGroup `json:"hooks"`
	}
	if json.Unmarshal(b, &config) != nil {
		return false
	}
	for gi, group := range config.Hooks[settings.EventPreToolUse] {
		if group.RunsGate(a, command, "Bash") && (a != "codex" || codexRTKTrusted(file, gi, group)) {
			return true
		}
	}
	return false
}

func codexRTKTrusted(file string, gi int, group settings.MatcherGroup) bool {
	config, err := os.ReadFile(filepath.Join(filepath.Dir(file), "config.toml"))
	if err != nil {
		return false
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(file))
	if err != nil {
		return false
	}
	for hi, hook := range group.Hooks {
		if !strings.Contains(hook.Command, "rtk-rewrite") {
			continue
		}
		handler := map[string]any{"type": hook.Type, "command": hook.Command, "timeout": max(1, hook.Timeout), "async": hook.Async}
		if hook.StatusMessage != "" {
			handler["statusMessage"] = hook.StatusMessage
		}
		identity := map[string]any{"event_name": "pre_tool_use", "hooks": []any{handler}}
		if group.Matcher != "" {
			identity["matcher"] = group.Matcher
		}
		canonical, err := textutil.MarshalNoEscape(identity)
		if err != nil {
			return false
		}
		key := fmt.Sprintf("%s:pre_tool_use:%d:%d", filepath.Join(parent, filepath.Base(file)), gi, hi)
		if codexTrustValue(config, key) == "sha256:"+textutil.SHA256Hex(canonical) {
			return true
		}
	}
	return false
}

func codexTrustValue(config []byte, key string) string {
	header := fmt.Sprintf("[hooks.state.%q]", key)
	for section := range strings.SplitSeq(string(config), "\n[") {
		if !strings.HasPrefix(strings.TrimSpace(section), strings.TrimPrefix(header, "[")) &&
			!strings.HasPrefix(strings.TrimSpace(section), header) {
			continue
		}
		for line := range strings.SplitSeq(section, "\n") {
			name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok && strings.TrimSpace(name) == "trusted_hash" {
				return strings.Trim(strings.TrimSpace(value), `"`)
			}
		}
	}
	return ""
}

func piShimsInstalled() bool {
	bin := rtkwrap.Binary()
	if paths.ExecutableRealPath(bin) == "" {
		return false
	}
	file, err := os.Open(filepath.Join(rtkwrap.Dir(), ".bash_env"))
	if err != nil {
		return false
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return false
	}
	for _, name := range append(slices.Clone(rtkwrap.Names), "rtk") {
		if target, err := os.Readlink(filepath.Join(rtkwrap.Dir(), name)); err != nil || target != bin {
			return false
		}
	}
	return true
}
