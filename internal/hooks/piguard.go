package hooks

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/settings"
)

// piGuardName is the "module" of `hook PreToolUse --pi-guard`: Pi's guard extension sends a raw tool call, this judges it.
const piGuardName = "pi-guard"

// piCall is the raw tool call the extension sends; Input is Pi's validated arguments, never forwarded to the hooks.
type piCall struct {
	Tool      string         `json:"tool"`
	Input     map[string]any `json:"input"`
	CWD       string         `json:"cwd"`
	SessionID string         `json:"session_id"`
}

// piVerdict is the whole reply: Block (a reason) blocks, only Allow runs the tool, on Path when it names one.
type piVerdict struct {
	Allow bool   `json:"allow,omitempty"`
	Block string `json:"block,omitempty"`
	Path  string `json:"path,omitempty"`
}

// piTools maps Pi's tool names to the Claude names the hooks match on.
var piTools = map[string]string{"bash": "Bash", "edit": "Edit", "write": "Write", "read": "Read", "grep": "Grep", "find": "Glob", "ls": "Read"}

// runPiGuard answers one raw Pi tool call on stdout; any failure to read or answer is a block, never an allow.
func runPiGuard(stdin io.Reader, stdout, stderr io.Writer) int {
	dropHookControlEnv() // fail closed: an inherited HARNESS_HOOK_SKIP must never turn the guards off
	var c piCall
	v := piVerdict{Block: "pi guard: unreadable tool call; blocked"}
	if raw, err := io.ReadAll(stdin); err == nil && json.Unmarshal(raw, &c) == nil {
		v = judgePi(c)
	}
	if err := json.NewEncoder(stdout).Encode(v); err != nil {
		_, _ = io.WriteString(stderr, "pi guard: verdict not written: "+err.Error()+"\n")
		return core.ExitBlock
	}
	return core.ExitProceed
}

// dropHookControlEnv unsets every hook-control variable of this process, whatever its case.
func dropHookControlEnv() {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); settings.IsHookControlEnv(k) {
			_ = proc.Unsetenv(k)
		}
	}
}

// judgePi normalizes the call, builds the hook payload from canonical fields only, and runs Pi's PreToolUse modules.
func judgePi(c piCall) piVerdict {
	tool, ok := piTools[c.Tool]
	if !ok {
		return piBlock("tool " + c.Tool + " is not mapped") // unknown and package tools are never allowed
	}
	ti, path, why := piToolInput(c)
	if why != "" {
		return piBlock(why)
	}
	payload, err := json.Marshal(map[string]any{"hook_event_name": settings.EventPreToolUse, "tool_name": tool, "tool_input": ti,
		"cwd": c.CWD, "session_id": c.SessionID, "harness_agent": "pi"})
	if err != nil {
		return piBlock("payload: " + err.Error())
	}
	for _, s := range settings.SpecsFor("pi", "plugin") {
		if s.Event != settings.EventPreToolUse || !matchTool(s.Matcher, tool) {
			continue
		}
		if reason := piModuleBlock(s, payload); reason != "" {
			return piVerdict{Block: reason}
		}
	}
	return piVerdict{Allow: true, Path: path}
}

func piBlock(why string) piVerdict { return piVerdict{Block: "pi guard: " + why + "; blocked"} }

// piModuleBlock is one module's block reason ("" allows); fail closed: a missing, failed or asking module blocks.
func piModuleBlock(s settings.EventSpec, payload []byte) string {
	run, ok := native[s.Name]
	if !ok {
		return "pi guard: " + s.Name + " is not available; blocked"
	}
	o := invokeDirect(s.Name, run, bytes.NewReader(payload), s)
	if o.in != nil {
		o.res = agentBashAllow(o.in, o.res) // the same owner-order loosening every agent's direct hook gets
	}
	switch {
	case o.res.Decision == core.Block:
		return reasonOr(o.res.Reason, "pi guard: "+s.Name+" blocked the call")
	case o.res.Decision == core.Ask:
		return reasonOr(o.res.Reason, "approval required; blocked") // no one to ask
	case o.err != nil:
		return "pi guard: " + s.Name + " did not complete (" + o.err.Error() + "); blocked"
	}
	return ""
}

func reasonOr(reason, fallback string) string {
	if strings.TrimSpace(reason) == "" {
		return fallback
	}
	return reason
}

// piToolInput is the Claude-shaped tool_input built from canonical fields only, and the path Pi must be handed.
func piToolInput(c piCall) (ti map[string]any, path, why string) {
	if c.Input == nil {
		return nil, "", "tool input is not an object"
	}
	if c.Tool == "bash" {
		cmd, _ := c.Input["command"].(string)
		if cmd == "" {
			return nil, "", "bash call has no command"
		}
		return map[string]any{"command": cmd}, "", ""
	}
	raw, ok := rawPath(c)
	if !ok {
		return nil, "", c.Tool + " call has no path"
	}
	path, why = resolvePiPath(raw, c.CWD)
	if why != "" {
		return nil, "", why
	}
	ti = map[string]any{"file_path": path}
	switch s, _ := c.Input["content"].(string); c.Tool {
	case "write":
		ti["content"] = s
	case "grep", "find":
		ti["pattern"], _ = c.Input["pattern"].(string)
	}
	return ti, path, ""
}

// rawPath is the call's path argument; grep, find and ls search the cwd when it is omitted, the others need one.
func rawPath(c piCall) (string, bool) {
	p, present := c.Input["path"]
	s, isStr := p.(string)
	switch {
	case present && p != nil && !isStr:
		return "", false // a path that is not a string is never guessed at
	case s != "":
		return s, true
	}
	return ".", c.Tool == "grep" || c.Tool == "find" || c.Tool == "ls"
}

// piUnicodeSpaces are the runes Pi's normalizePath turns into ASCII spaces (UNICODE_SPACES in Pi's utils/paths.js).
const piUnicodeSpaces = "              　"

// normalizePiPath mirrors Pi 1.6.0's normalizePath: Unicode spaces to " ", one leading "@" dropped, "~" and "~/" expanded.
func normalizePiPath(raw string) (string, string) {
	p := strings.Map(func(r rune) rune {
		if strings.ContainsRune(piUnicodeSpaces, r) {
			return ' '
		}
		return r
	}, raw)
	p = strings.TrimPrefix(p, "@")
	switch {
	case strings.ContainsRune(p, 0):
		return "", "the path contains a NUL byte"
	case runtime.GOOS == "windows" && strings.HasPrefix(p, "/"):
		return "", "Pi rewrites a leading / on Windows; not reimplemented"
	case strings.HasPrefix(p, "file://"):
		return "", "file:// paths are not accepted" // Pi turns them into another path; not reimplemented
	case p == "~" || strings.HasPrefix(p, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "the home directory is unknown"
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/")), ""
	}
	return p, ""
}

// resolvePiPath is the absolute path Pi's resolveToCwd gives raw; a form it cannot reproduce exactly is a block reason.
func resolvePiPath(raw, cwd string) (path, why string) {
	if !filepath.IsAbs(cwd) || strings.ContainsRune(cwd, 0) {
		return "", "the working directory is not an absolute path"
	}
	p, why := normalizePiPath(raw)
	if why != "" {
		return "", why
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	p = filepath.Clean(p)
	if strings.ContainsAny(p, piUnicodeSpaces) {
		return "", "the resolved path holds a non-ASCII space Pi would rewrite" // Pi's own pass must be a no-op on it
	}
	return p, ""
}
