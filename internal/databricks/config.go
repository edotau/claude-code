package databricks

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/databricks/oauth"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// cfgFlags are the flags `config` and `rescue` share.
type cfgFlags struct {
	profile, host, wire, auth, out, model string
	force                                 bool
	set, unset                            multi
}

var configPage = helptext.Page{
	Usage: "claude-code databricks config <claude|codex> [flags]",
	About: "Render a ready-to-use Databricks config from embedded templates. " +
		"Prints to stdout (never a token); --out writes it 0600.",
}

var rescuePage = helptext.Page{
	Usage: "claude-code databricks rescue [flags] [-- claude args...]",
	About: "Break-glass: run claude with a scratch settings file that layers on top of " +
		"your user settings; no router and no harness overlay. The file is deleted when claude exits. " +
		"--print shows the file and command only and leaves the file.",
}

func bind(fs *flag.FlagSet, f *cfgFlags, authDef string, withOut bool) {
	profileFlagVar(fs, &f.profile, profileUsage)
	fs.StringVar(&f.host, "host", "", "https://host override, skips the vault")
	fs.StringVar(&f.wire, "wire", "", "ai-gateway or serving (default: the row's, else serving)")
	fs.StringVar(&f.model, "model", "", "model override, canonical or wire id")
	fs.StringVar(&f.auth, "auth", authDef, "harness (token helper) or cli (databricks CLI)")
	fs.Var(&f.set, "set", "K=V env key to add or change (repeatable)")
	fs.Var(&f.unset, "unset", "K env key to remove (repeatable)")
	if withOut {
		fs.StringVar(&f.out, "out", "", "write to FILE (0600) instead of stdout")
		fs.BoolVar(&f.force, "force", false, "overwrite an existing --out file")
	}
}

// resolve returns the expanded provider row and its https host: --host builds a preset row in memory,
// else the registry row (re-expanded from its host when --wire differs from the row's).
func resolve(f cfgFlags) (*providers.Provider, string, error) {
	reg, lerr := providers.Load() // nil reg: --host alone still renders, under the generic name
	name := resolveProfile(reg, f.profile)
	if name == "" {
		name = providers.GroupDatabricks
	}
	if f.wire != "" && f.wire != providers.WireServing && f.wire != providers.WireAIGateway {
		return nil, "", fmt.Errorf("--wire must be ai-gateway or serving (got %q)", f.wire)
	}
	host, err := flagHost(f.host)
	if err != nil {
		return nil, "", err
	}
	var row *providers.Provider
	if host == "" {
		if row, host, err = registryHost(reg, lerr, name); err != nil {
			return nil, "", err
		}
	}
	if row != nil && rowServesWire(row, f.wire) {
		return row, host, nil
	}
	return synth(name, host, f.wire)
}

// flagHost normalizes a --host value; "" stays "" (use the registry).
func flagHost(h string) (string, error) {
	if h == "" {
		return "", nil
	}
	if n := oauth.NormalizeHost(h); n != "" {
		return n, nil
	}
	return "", fmt.Errorf("host %q is not a valid https workspace URL", h)
}

// registryHost returns the registry row for name and its https workspace host.
func registryHost(reg *providers.Registry, lerr error, name string) (*providers.Provider, string, error) {
	if reg == nil {
		return nil, "", fmt.Errorf("cannot read the registry (%v); pass --host URL", lerr)
	}
	row := reg.Providers[name]
	if row == nil {
		return nil, "", fmt.Errorf("no provider %q; pass --host URL", name)
	}
	for _, raw := range []string{row.Host, row.BaseURL} {
		if u, err := url.Parse(raw); err == nil && u.Scheme == "https" && u.Host != "" {
			return row, "https://" + u.Host, nil
		}
	}
	return nil, "", fmt.Errorf("provider %q has no host; pass --host URL", name)
}

// rowServesWire reports whether the registry row already uses the requested wire (empty means any).
func rowServesWire(row *providers.Provider, wire string) bool {
	return wire == "" || wire == row.Options.Wire || (wire == providers.WireServing && row.Options.Wire == "")
}

// synth expands a bare databricks preset row for host and wire through the registry's own loader.
func synth(name, host, wire string) (*providers.Provider, string, error) {
	doc, err := json.Marshal(map[string]any{"providers": map[string]any{providers.GroupDatabricks: map[string]any{name: map[string]any{
		"preset": providers.PresetDatabricks, "host": host, "options": map[string]string{"wire": wire}}}}})
	if err != nil {
		return nil, "", err
	}
	reg, _ := providers.Parse([]byte("{}"), doc) // a reg is always returned; the row check below is the gate
	p := reg.Providers[name]
	if p == nil || p.BaseURL == "" {
		return nil, "", fmt.Errorf("host %q is not a valid https workspace URL", host)
	}
	return p, host, nil
}

func (f cfgFlags) options(target, self string) (renderOptions, error) {
	p, host, err := resolve(f)
	if err != nil {
		return renderOptions{}, err
	}
	return renderOptions{Target: target, Host: host, P: p, Auth: f.auth, Self: self,
		Model: f.model, Set: f.set, Unset: f.unset}, nil
}

// config is `databricks config <claude|codex>`.
func config(args []string) int {
	if len(args) == 0 || helptext.WantsHelp(args[:1]) {
		configUsage()
		return 0
	}
	target := args[0]
	var f cfgFlags
	fs := helptext.NewFlagSet("claude-code databricks config", configPage)
	bind(fs, &f, authHarness, true)
	if code, done := helptext.Parse(fs, args[1:]); done {
		return code
	}
	self, _ := os.Executable()
	o, err := f.options(target, self)
	if err == nil {
		var res renderResult
		if res, err = renderConfig(o); err == nil {
			return emit(f, target, res)
		}
	}
	fmt.Fprintf(os.Stderr, "claude-code databricks config: %v\n", err)
	return 2
}

func emit(f cfgFlags, target string, res renderResult) int {
	if res.Note != "" {
		fmt.Fprintln(os.Stderr, "note: "+res.Note)
	}
	if f.out == "" {
		_, _ = io.WriteString(os.Stdout, res.Text)
		return 0
	}
	if err := writeFile0600(f.out, res.Text, f.force); err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks config: %v\n", err)
		return 2
	}
	fmt.Fprintf(os.Stderr, "wrote %s config to %s\n", target, f.out)
	return 0
}

func configUsage() {
	fs := helptext.NewFlagSet("claude-code databricks config", configPage)
	bind(fs, &cfgFlags{}, authHarness, true)
	fs.Usage()
}

// findClaude locates the claude binary: the harness agent link, else PATH.
func findClaude() (string, error) {
	if p, err := paths.AgentBin("claude"); err == nil {
		return p, nil
	}
	return exec.LookPath("claude")
}

// scrubPrefixes and scrubNames are the inherited vars rescue drops so only the scratch settings steer claude.
var (
	scrubPrefixes = []string{"HARNESS_", "ANTHROPIC_", "CLAUDE_CODE_USE_", "DATABRICKS_"}
	scrubNames    = []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR"}
)

// scrubbed drops proc.ScrubDeny plus every inherited var matching scrubPrefixes or scrubNames.
func scrubbed(environ []string) []string {
	environ, _ = proc.Scrub(environ, nil)
	var out []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if slices.Contains(scrubNames, name) || slices.ContainsFunc(scrubPrefixes, func(p string) bool {
			return strings.HasPrefix(name, p)
		}) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// rescue is `databricks rescue`: scratch settings file, then claude --settings, no router and no overlay.
func rescue(args []string) int {
	if len(args) > 0 && helptext.WantsHelp(args[:1]) {
		rescueUsage()
		return 0
	}
	var f cfgFlags
	var print bool
	fs := helptext.NewFlagSet("claude-code databricks rescue", rescuePage)
	bind(fs, &f, authCLI, false)
	fs.BoolVar(&print, "print", false, "print the file and command, do not run")
	if code, done := helptext.Parse(fs, args); done {
		return code
	}
	res, file, err := renderScratch(f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks rescue: %v\n", err)
		return 2
	}
	if res.Note != "" {
		fmt.Fprintln(os.Stderr, "note: "+res.Note)
	}
	argv := append([]string{"--settings", file}, fs.Args()...)
	bin, ferr := findClaude()
	if print {
		fmt.Printf("# %s\n%s\n# command\nclaude %s\n", file, res.Text, strings.Join(argv, " "))
		return 0
	}
	if ferr != nil {
		_ = os.Remove(file)
		fmt.Fprintf(os.Stderr, "claude-code databricks rescue: claude not found: %v\n", ferr)
		return 2
	}
	fmt.Fprintf(os.Stderr, "running: %s %s\n", bin, strings.Join(argv, " "))
	code, err := spawn(bin, argv, scrubbed(os.Environ()), os.Stdin, os.Stdout, os.Stderr)
	_ = os.Remove(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks rescue: exec failed: %v\n", err)
		return 2
	}
	return code
}

// renderScratch renders the claude config for f and writes it to a scratch file.
func renderScratch(f cfgFlags) (renderResult, string, error) {
	self, _ := os.Executable()
	o, err := f.options(targetClaude, self)
	if err != nil {
		return renderResult{}, "", err
	}
	res, err := renderConfig(o)
	if err != nil {
		return renderResult{}, "", err
	}
	file, err := writeScratch(res.Text)
	return res, file, err
}

// writeScratch writes text to a fresh 0600 file under the OS temp dir.
func writeScratch(text string) (string, error) {
	w, err := os.CreateTemp("", "dbx-rescue-*.json")
	if err != nil {
		return "", err
	}
	name := w.Name()
	_, err = w.WriteString(text)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, 0o600)
	}
	if err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return filepath.Clean(name), nil
}

func rescueUsage() {
	fs := helptext.NewFlagSet("claude-code databricks rescue", rescuePage)
	bind(fs, &cfgFlags{}, authCLI, false)
	fs.Bool("print", false, "print the file and command, do not run")
	fs.Usage()
}

const (
	probeTimeout   = 15 * time.Second // one max_tokens=1 ping
	profileTimeout = 20 * time.Second // one profile's token plus its pings
	checkWorkers   = 3                // profiles probed at once
	budgetDenied   = "budget denied"
)

var checkAllPage = helptext.Page{
	Usage: "claude-code databricks check --all [flags]",
	About: "One tiny Anthropic call per profile on the serving wire (--wire all adds\n" +
		"ai-gateway); prints a status per cell, never a token. Exit 1 when a cell\n" +
		"fails for any reason other than a budget denial.",
}

// probeClient is the HTTP client for probes.
var probeClient = &http.Client{Timeout: probeTimeout}

// checkWires maps --wire to the wires probed and their column headers.
func checkWires(wire string) ([]string, []string, error) {
	switch wire {
	case providers.WireServing:
		return []string{providers.WireServing}, []string{"SERVING"}, nil
	case providers.WireAIGateway:
		return []string{providers.WireAIGateway}, []string{"AI-GATEWAY"}, nil
	case "all":
		return []string{providers.WireServing, providers.WireAIGateway}, []string{"SERVING", "AI-GATEWAY"}, nil
	}
	return nil, nil, fmt.Errorf("--wire must be serving, ai-gateway or all (got %q)", wire)
}

// checkRow is one profile's probe outcome.
type checkRow struct {
	cells         []string
	reason, notes string
}

// checkAll is `databricks check --all`: the profile x wire matrix (one profile with --profile).
func checkAll(args []string) int {
	fs := helptext.NewFlagSet("databricks check --all", checkAllPage)
	model := fs.String("model", aliasID(models.Haiku), "canonical model id to probe")
	wire := fs.String("wire", providers.WireServing, "serving, ai-gateway or all")
	only := new(string)
	profileFlagVar(fs, only, "probe only this Databricks profile (default: all)")
	rest, err := helptext.ParseArgs(fs, args)
	if err != nil {
		return helptext.ExitCode(err)
	}
	if len(rest) > 1 {
		fs.Usage()
		return 2
	}
	if len(rest) == 1 { // positional MODEL overrides --model
		*model = rest[0]
	}
	wires, heads, err := checkWires(*wire)
	if err != nil {
		return fail("check: %v", err)
	}
	reg, err := providers.Load()
	if err != nil {
		return fail("check: %v", err)
	}
	names := providers.DatabricksProfiles(reg)
	if *only != "" {
		names = []string{*only}
	}
	return printCheck(names, heads, checkProfiles(reg, names, *model, wires))
}

// printCheck prints the table, then reasons and SP notes under it; 1 when a cell failed other than a budget denial.
func printCheck(names, heads []string, rows []checkRow) int {
	fmt.Println(checkLine("PROFILE", heads))
	rc := 0
	var why []string
	for i, name := range names {
		r := rows[i]
		for _, c := range r.cells {
			if c != "ok" && c != budgetDenied {
				rc = 1
			}
		}
		fmt.Println(checkLine(name, r.cells))
		if r.reason != "" {
			why = append(why, fmt.Sprintf("  %s: %s", name, r.reason))
		}
		if r.notes != "" {
			why = append(why, "  "+strings.ReplaceAll(strings.TrimSpace(r.notes), "\n", "\n  "))
		}
	}
	if len(why) > 0 {
		fmt.Println()
		fmt.Println(strings.Join(why, "\n"))
	}
	return rc
}

// checkLine is one table row: the label then each cell in a 24-wide column, the last unpadded.
func checkLine(label string, cells []string) string {
	cols := append([]string{label}, cells...)
	for i := range len(cols) - 1 {
		cols[i] = fmt.Sprintf("%-24s", cols[i])
	}
	return strings.Join(cols, " ")
}

// checkProfiles probes names concurrently, checkWorkers at a time; rows keep the names' order.
func checkProfiles(reg *providers.Registry, names []string, model string, wires []string) []checkRow {
	rows := make([]checkRow, len(names))
	sem := make(chan struct{}, checkWorkers)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			rows[i] = checkProfile(reg.Providers[name], name, model, wires)
		})
	}
	wg.Wait()
	return rows
}

// checkProfile mints the row's token once and probes every wire with it; reason is the full cause when no wire ran.
func checkProfile(row *providers.Provider, name, model string, wires []string) checkRow {
	r := checkRow{cells: make([]string, len(wires))}
	fill := func(s, why string) checkRow {
		for i := range r.cells {
			r.cells[i] = s
		}
		r.reason = why
		return r
	}
	if row == nil {
		return fill("no row", "not in the registry")
	}
	host := providers.AuthHost(row)
	if host == "" {
		return fill("no https host", "the profile has no https host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), profileTimeout)
	defer cancel()
	var notes noteBuf
	ctx = oauth.WithoutRetry(providers.WithNotes(ctx, notes.add)) // a check reports a 429, it never waits one out
	tok, err := providers.Credential(ctx, row)
	r.notes = notes.String()
	if err != nil {
		return fill("no token", strings.TrimPrefix(err.Error(), name+": "))
	}
	var wg sync.WaitGroup
	for i, w := range wires {
		wg.Go(func() { r.cells[i] = probeWire(ctx, row, name, host, w, tok, model) })
	}
	wg.Wait()
	return r
}

// noteBuf collects credential notes; a detached mint may add from another goroutine.
type noteBuf struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (n *noteBuf) add(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.buf.WriteString(s + "\n")
}

func (n *noteBuf) String() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.buf.String()
}

// probeWire probes row on wire w, synthesizing the row for a wire other than its own.
func probeWire(ctx context.Context, row *providers.Provider, name, host, w, tok, model string) string {
	p := row
	if w != row.Options.Wire && (w != providers.WireServing || row.Options.Wire != "") {
		var err error
		if p, _, err = synth(name, host, w); err != nil {
			return clip(err.Error(), 40)
		}
	}
	return probe(ctx, p, tok, model)
}

// probe sends one max_tokens=1 message on p's Anthropic route and classifies the reply.
func probe(ctx context.Context, p *providers.Provider, tok, model string) string {
	base, ok := p.Route(providers.ProtocolAnthropic)
	if !ok {
		return "no anthropic route"
	}
	body, _ := json.Marshal(map[string]any{"model": p.WireModel(model), "max_tokens": 1,
		"messages": []map[string]string{{"role": "user", "content": "ping"}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/messages",
		bytes.NewReader(body))
	if err != nil {
		return "bad request"
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return probeFailure(ctx, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil { // a truncated body is a failure even on 2xx
		return fmt.Sprintf("HTTP %d (body cut off)", resp.StatusCode)
	}
	return classify(resp.StatusCode, strings.ReplaceAll(string(raw), tok, "***"))
}

// probeFailure names a transport failure by kind; the error text itself could echo the URL.
func probeFailure(ctx context.Context, err error) string {
	var ne net.Error
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return "network error: timeout"
	case errors.As(err, new(*net.DNSError)):
		return "network error: DNS"
	case errors.As(err, new(*tls.CertificateVerificationError)):
		return "network error: TLS"
	}
	return "network error"
}

// classify names a probe outcome: ok, budget denied (403 mentioning budget), else HTTP <code>: <message>.
func classify(status int, body string) string {
	if status >= 200 && status < 300 {
		return "ok"
	}
	if status == http.StatusForbidden && strings.Contains(strings.ToLower(body), "budget") {
		return budgetDenied
	}
	var e struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(body)
	if json.Unmarshal([]byte(body), &e) == nil {
		msg = strings.TrimSpace(e.Error.Message + " " + e.Message)
	}
	if msg == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return clip(fmt.Sprintf("HTTP %d: %s", status, msg), 48)
}

// clip cuts s to n runes on one line.
func clip(s string, n int) string {
	return textutil.Truncate(strings.Join(strings.Fields(s), " "), n)
}
