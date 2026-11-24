package hooks

import (
	"bytes"
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
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/contract"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/paths"
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

// piVerdict is the whole reply: Block blocks, only Allow runs the tool (on Path or Run); Usage is a tool error.
type piVerdict struct {
	Allow  bool     `json:"allow,omitempty"`
	Block  string   `json:"block,omitempty"`
	Path   string   `json:"path,omitempty"`
	Run    []string `json:"run,omitempty"`
	Usage  string   `json:"usage,omitempty"`
	Notice string   `json:"notice,omitempty"` // one line the tool result carries: the contract budget notice
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

// judgePi judges the call, then adds the owner's budget notice; a dispatch at the budget runs nothing.
func judgePi(c piCall) piVerdict {
	v := judgePiCall(c)
	if !v.Allow {
		return v
	}
	leaves, tokens := piBudget()
	switch {
	case c.Tool == "dispatch" && (leaves || tokens):
		return piVerdict{Allow: true, Notice: contract.BudgetNotice} // a tool result, not an error
	case tokens:
		v.Notice = contract.BudgetNotice
	}
	return v
}

// piBudget reports whether this run's owner contract dispatched every leaf, and whether it is past its tokens.
func piBudget() (leaves, tokens bool) {
	c, ok := heldContract(os.Getenv(proc.DispatchIDEnv))
	if !ok || c.Owner.Role != contract.RoleOwner {
		return false, false
	}
	rec, err := agent.LoadRecord(c.ID)
	if err != nil {
		return false, false // a notice only: an unreadable record never blocks
	}
	s := contract.SpentOf(c, dispatchUses(rec.Time))
	return c.LeavesSpent(s), c.TokensSpent(s)
}

// heldContract is the open contract id holds: its own, else one it took over by a re-assign.
func heldContract(id string) (contract.Contract, bool) {
	if id == "" {
		return contract.Contract{}, false
	}
	c, err := contract.Load(id)
	if errors.Is(err, fs.ErrNotExist) {
		open, _ := contract.Pending() // an unreadable contract is skipped: a notice never blocks
		i := slices.IndexFunc(open, func(p contract.Contract) bool { return slices.Contains(p.Holders, id) })
		if i < 0 {
			return contract.Contract{}, false
		}
		c, err = open[i], nil
	}
	return c, err == nil && !c.State().Terminal()
}

// budgetRecord is the part of a dispatch record the budget sums; the rest of the record is never decoded.
type budgetRecord struct {
	ID       string      `json:"id"`
	Time     time.Time   `json:"time"`
	Parent   string      `json:"parent"`
	Usage    *wire.Usage `json:"usage"`
	Activity *struct {
		Usage wire.Usage `json:"usage"`
	} `json:"activity"`
}

// dispatchUses reads every dispatch record started since since, read-only: no record is reaped or rewritten.
func dispatchUses(since time.Time) []contract.Use {
	dir := paths.DispatchDir()
	ents, _ := os.ReadDir(dir) // no dir: nothing spent
	var out []contract.Use
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil || !strings.HasSuffix(e.Name(), ".json") || fi.ModTime().Before(since) {
			continue // a record is written at or after its start, so an older file is no subtree member
		}
		var r budgetRecord
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err != nil || json.Unmarshal(b, &r) != nil ||
			r.Time.Before(since) {
			continue
		}
		out = append(out, contract.Use{ID: r.ID, Parent: r.Parent, Tokens: r.tokens()})
	}
	return out
}

// tokens is the record's final usage, else (still running) its live usage so far.
func (r budgetRecord) tokens() int {
	u := r.Usage
	if u == nil && r.Activity != nil {
		u = &r.Activity.Usage
	}
	if u == nil {
		return 0
	}
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheCreationTokens
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
		payload, err := json.Marshal(map[string]any{"hook_event_name": settings.EventPreToolUse, "tool_name": tool, "tool_input": ti,
			"cwd": c.CWD, "session_id": c.SessionID, "harness_agent": "pi"})
		if err != nil {
			return piBlock("payload: " + err.Error())
		}
		if reason := piModulesBlock(settings.SpecsFor("pi", "plugin"), tool, payload); reason != "" {
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

// piModulesBlock runs the PreToolUse modules matching tool; fail closed: none judging the call is a block, not an allow.
func piModulesBlock(specs []settings.EventSpec, tool string, payload []byte) string {
	judged := 0
	for _, s := range specs {
		if s.Event != settings.EventPreToolUse || !matchTool(s.Matcher, tool) {
			continue
		}
		judged++
		if reason := piModuleBlock(s, payload); reason != "" {
			return reason
		}
	}
	if judged == 0 {
		return "pi guard: no guard module matched " + tool + "; blocked"
	}
	return ""
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

// piModuleBlock is one module's block reason ("" allows); fail closed: a missing, failed or asking module blocks.
func piModuleBlock(s settings.EventSpec, payload []byte) string {
	run, ok := native[s.Name]
	if !ok {
		return "pi guard: " + s.Name + " is not available; blocked"
	}
	o := invokeDirect(s.Name, run, bytes.NewReader(payload), s)
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

// piHandbackKeys and piGateKeys are the typed arguments of the two runner actions.
var (
	piHandbackKeys = []string{"status", "summary", "unfixed", "tests", "method"}
	piGateKeys     = []string{"scope"}
	piScopeEntry   = regexp.MustCompile(`^[A-Za-z0-9._/@+-]+$`)
)

// isPiAction reports whether tool is a runner action: it names no path and runs at most one harness argv.
func isPiAction(tool string) bool { return tool == "gate" || tool == "handback" }

// piActionArgv is the exact claude-code argv an action runs, built from its typed input; nil: nothing to run.
func piActionArgv(tool string, input map[string]any) ([]string, error) {
	keys := piGateKeys
	if tool == "handback" {
		keys = piHandbackKeys
	}
	for k := range input {
		if !slices.Contains(keys, k) {
			return nil, fmt.Errorf("%s: unknown argument %q (allowed: %s)", tool, k, strings.Join(keys, ", "))
		}
	}
	if tool == "handback" {
		return nil, nil
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
	argv, err := piActionArgv(c.Tool, c.Input)
	if err != nil {
		return piVerdict{Usage: err.Error()}
	}
	if argv == nil {
		return piVerdict{Allow: true}
	}
	payload, err := json.Marshal(map[string]any{"hook_event_name": settings.EventPreToolUse, "tool_name": "Bash",
		"tool_input": map[string]any{"command": "claude-code " + strings.Join(argv, " ")}, "cwd": c.CWD,
		"session_id": c.SessionID, "harness_agent": "pi"})
	if err != nil {
		return piBlock("payload: " + err.Error())
	}
	if reason := piModulesBlock(settings.SpecsFor("pi", "plugin"), "Bash", payload); reason != "" {
		return piVerdict{Block: reason}
	}
	return piVerdict{Allow: true, Run: argv}
}
