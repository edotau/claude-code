// Package pi judges Pi's raw tool calls and runs its owner verbs; the module table comes in through Guard.
package pi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/dev/contract"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/settings"
	"github.com/edotau/claude-code/internal/platform/textutil"
)

// GuardName is the "module" of `hook PreToolUse --pi-guard`: Pi's guard sends a raw tool call, this judges it.
const GuardName = "pi-guard"

// piCall is the raw tool call the extension sends; Input is Pi's validated arguments, never forwarded to the hooks.
type piCall struct {
	Tool      string         `json:"tool"`
	Input     map[string]any `json:"input"`
	CWD       string         `json:"cwd"`
	SessionID string         `json:"session_id"`

	modules func(tool string, payload []byte) string // the PreToolUse modules' block reason, set by Guard
}

// piVerdict is the whole reply: Block blocks, only Allow runs the tool (on Path or Run); Usage is a tool error.
type piVerdict struct {
	Allow  bool     `json:"allow,omitempty"`
	Block  string   `json:"block,omitempty"`
	Path   string   `json:"path,omitempty"`
	Run    []string `json:"run,omitempty"`
	Usage  string   `json:"usage,omitempty"`
	Notice string   `json:"notice,omitempty"` // one line the tool result carries: the contract budget notice
	Text   string   `json:"text,omitempty"`   // the verify result: the fixing turn's redacted text, "" when it passed
	Stdin  string   `json:"stdin,omitempty"`  // the text the Run argv reads on stdin: a report's body
}

// piTools maps Pi's tool names to the Claude names the hooks match on.
var piTools = map[string]string{"bash": "Bash", "edit": "Edit", "write": "Write", "read": "Read", "grep": "Grep", "find": "Glob", "ls": "Read"}

// Guard answers one raw Pi tool call on stdout; any failure to read or answer is a block, never an allow.
func Guard(stdin io.Reader, stdout, stderr io.Writer, modules func(tool string, payload []byte) string) int {
	dropHookControlEnv() // fail closed: an inherited HARNESS_HOOK_SKIP must never turn the guards off
	var c piCall
	v := piVerdict{Block: "pi guard: unreadable tool call; blocked"}
	dec := json.NewDecoder(stdin)
	if dec.Decode(&c) == nil {
		c.modules = modules
		var extra json.RawMessage
		if c.Tool == "verify" { // the rest of stdin is the liveness pipe: its EOF means Pi is gone
			v = runPiVerify(c, io.MultiReader(dec.Buffered(), stdin))
		} else if dec.Decode(&extra) == io.EOF { // only JSON whitespace may follow, as json.Unmarshal required
			v = judgePi(c)
		}
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

// judgePi judges the call, then adds the owner's budget notice; a dispatch at the budget runs nothing.
func judgePi(c piCall) piVerdict {
	v := judgePiCall(c)
	if !v.Allow {
		return v
	}
	own, err := piOwner()
	if err != nil {
		return v
	}
	s := wire.SpentBy(own)
	leaves, tokens := own.LeavesSpent(s), own.TokensSpent(s)
	switch {
	case c.Tool == "dispatch" && (leaves || tokens):
		return piVerdict{Allow: true, Notice: contract.BudgetNotice} // a tool result, not an error
	case tokens:
		v.Notice = contract.BudgetNotice
	case c.Tool == "check" || c.Tool == "dispatch":
		v.Notice = piResent(own)
	}
	return v
}

// heldContract is the open contract id holds: its own, else one it took over by a re-assign.
func heldContract(id string) (contract.Contract, bool) {
	if id == "" {
		return contract.Contract{}, false
	}
	for _, try := range append([]string{id}, agent.ResumeChain(id)...) { // a retry: the dispatches it resumed
		c, err := contract.Load(try)
		if errors.Is(err, fs.ErrNotExist) {
			open, _ := contract.Pending() // an unreadable contract is skipped: a notice never blocks
			if i := slices.IndexFunc(open, func(p contract.Contract) bool { return slices.Contains(p.Holders, try) }); i >= 0 {
				c, err = open[i], nil
			}
		}
		if err == nil {
			return c, !c.State().Terminal()
		}
	}
	return contract.Contract{}, false
}

// piResent is the notice for owner c's re-sent leaf: its id and the dispatch now holding it, "" for none.
func piResent(c contract.Contract) string {
	var out []string
	for _, id := range c.Children {
		if k, err := contract.Load(id); err == nil && k.State() == contract.StateWorking && len(k.Holders) > 1 {
			out = append(out, id+" re-sent as "+k.Holders[len(k.Holders)-1])
		}
	}
	if len(out) == 0 {
		return ""
	}
	return "leaf died and was re-sent once: " + strings.Join(out, ", ")
}

// judgePiCall normalizes the call, builds the payload from canonical fields only, and runs Pi's PreToolUse modules.
func judgePiCall(c piCall) piVerdict {
	if isPiAction(c.Tool) {
		return judgePiAction(c)
	}
	tool, ok := piTools[c.Tool]
	if !ok {
		return piBlock("tool " + c.Tool + " is not mapped") // unknown and package tools are never allowed
	}
	ti, path, why := piToolInput(c)
	if why != "" {
		return piBlock(why)
	}
	spellings, why := piSpellings(path)
	if why != "" {
		return piBlock(why)
	}
	for _, sp := range spellings {
		if sp != "" {
			ti["file_path"] = sp
		}
		if reason := piModulesBlock(c, tool, ti); reason != "" {
			return piVerdict{Block: reason}
		}
	}
	return piVerdict{Allow: true, Path: path}
}

// piSpellings is the path as typed and, when it differs, as the disk spells it: one file, so each must pass the hooks.
func piSpellings(path string) ([]string, string) {
	if path == "" {
		return []string{""}, "" // bash names no path
	}
	disk, why := diskSpelling(path)
	if why != "" || disk == path {
		return []string{path}, why
	}
	return []string{path, disk}, ""
}

// diskSpelling is p with each existing component named as its directory lists it; fail closed on what it cannot know.
func diskSpelling(p string) (string, string) {
	vol := filepath.VolumeName(p)
	cur := vol + string(filepath.Separator)
	parts := strings.Split(strings.Trim(p[len(vol):], string(filepath.Separator)), string(filepath.Separator))
	for i, part := range parts {
		fi, err := os.Lstat(filepath.Join(cur, part))
		if err != nil {
			tail := filepath.Join(parts[i:]...)
			if strings.ContainsFunc(tail, func(r rune) bool { return r >= 0x300 && r <= 0x36F }) {
				return "", "a new path with a combining mark has no known on-disk spelling" // NFC and NFD could be one file
			}
			return filepath.Join(cur, tail), ""
		}
		ents, err := os.ReadDir(cur)
		if err != nil {
			return "", "the directory " + cur + " cannot be listed for its on-disk spelling"
		}
		name := part
		exact := slices.ContainsFunc(ents, func(e os.DirEntry) bool { return e.Name() == part })
		for _, e := range ents { // stat entries only when no entry has the typed name
			if ei, err := e.Info(); !exact && err == nil && os.SameFile(ei, fi) {
				name = e.Name()
				break
			}
		}
		cur = filepath.Join(cur, name)
	}
	return cur, ""
}

func piBlock(why string) piVerdict { return piVerdict{Block: "pi guard: " + why + "; blocked"} }

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
	case strings.HasPrefix(strings.ToLower(p), "file://"):
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

// piHandbackKeys, piGateKeys and piReportKeys are the typed arguments of the runner actions.
var (
	piHandbackKeys = []string{"status", "summary", "unfixed", "tests", "method"}
	piGateKeys     = []string{"scope"}
	piReportKeys   = []string{"kind", "body"}
	piScopeEntry   = regexp.MustCompile(`^[A-Za-z0-9._/@+-]+$`)
)

// isPiAction reports whether tool is a runner action: it names no path and runs at most one harness argv.
func isPiAction(tool string) bool {
	return slices.Contains([]string{"gate", "handback", "dispatch", "check", "report"}, tool)
}

// piActionArgv is the exact claude-code argv an action runs, built from its typed input; nil: nothing to run.
func piActionArgv(tool string, input map[string]any, cwd string) ([]string, error) {
	keys := map[string][]string{"gate": piGateKeys, "handback": piHandbackKeys, "dispatch": piDispatchKeys, "check": piCheckKeys,
		"report": piReportKeys}[tool]
	for k := range input {
		if !slices.Contains(keys, k) {
			return nil, fmt.Errorf("%s: unknown argument %q (allowed: %s)", tool, k, strings.Join(keys, ", "))
		}
	}
	switch tool {
	case "handback":
		return nil, nil
	case "dispatch":
		return piDispatchArgv(input, cwd)
	case "check":
		return piCheckArgv(input)
	case "report":
		return piReportArgv(input)
	}
	argv := []string{"worktree", "gate", "--run", "--format", "json"}
	scope, err := piGateScope(input["scope"])
	if err != nil {
		return nil, err
	}
	if scope == "" {
		return argv, nil
	}
	return append(argv, "--scope", scope), nil
}

// piReportArgv files the body as the run's own report; the model supplies neither the dispatch id nor the argv.
func piReportArgv(input map[string]any) ([]string, error) {
	kind, body, id := strOf(input["kind"]), strOf(input["body"]), os.Getenv(proc.DispatchIDEnv)
	if !slices.Contains(memory.ReportKinds, kind) {
		return nil, fmt.Errorf("report: kind must be one of %s", strings.Join(memory.ReportKinds, ", "))
	}
	if strings.TrimSpace(body) == "" {
		return nil, errors.New("report: body must be a non-empty string")
	}
	if id == "" {
		return nil, errors.New("report: no dispatch to file the report on")
	}
	return []string{"memory", "report", "--job", id, "--kind", kind, "--stdin"}, nil
}

// piGateScope is the comma-joined scope dirs; each must be a clean relative directory.
func piGateScope(list any) (string, error) {
	if list == nil {
		return "", nil
	}
	items, ok := list.([]any)
	if !ok {
		return "", errors.New("gate: scope must be an array of directories")
	}
	var scope []string
	for _, it := range items {
		s, _ := it.(string)
		if !filepath.IsLocal(s) || !piScopeEntry.MatchString(s) {
			return "", fmt.Errorf("gate: scope entry %q is not a clean relative directory", s)
		}
		scope = append(scope, s)
	}
	return strings.Join(scope, ","), nil
}

// judgePiAction judges a runner action: a gate is judged as the bash line it will run, a handback runs nothing.
func judgePiAction(c piCall) piVerdict {
	argv, err := piActionArgv(c.Tool, c.Input, c.CWD)
	if err != nil {
		return piVerdict{Usage: err.Error()}
	}
	if argv == nil {
		return piVerdict{Allow: true}
	}
	if reason := piModulesBlock(c, "Bash", map[string]any{"command": "claude-code " + textutil.ShellJoin(argv)}); reason != "" {
		return piVerdict{Block: reason}
	}
	v := piVerdict{Allow: true, Run: argv}
	if c.Tool == "report" {
		v.Stdin = strOf(c.Input["body"])
	}
	return v
}

// piModulesBlock runs Pi's PreToolUse modules for tool on tool_input ti; "" allows.
func piModulesBlock(c piCall, tool string, ti map[string]any) string {
	payload, err := json.Marshal(map[string]any{"hook_event_name": settings.EventPreToolUse, "tool_name": tool, "tool_input": ti,
		"cwd": c.CWD, "session_id": c.SessionID, "harness_agent": "pi"})
	if err != nil {
		return piBlock("payload: " + err.Error()).Block
	}
	return c.modules(tool, payload)
}
