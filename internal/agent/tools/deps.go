package tools

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// depClass says what breaks when a host tool is missing.
type depClass string

const (
	depRequired depClass = "required" // the build or a core verb fails without it
	depFeature  depClass = "feature"  // one feature degrades or is skipped
	depGate     depClass = "gate"     // a lint or audit gate skips
)

// dep is one host tool the harness shells out to; no Brew/Go/Npm/Py route makes the row report-only.
type dep struct {
	Name    string // executable name
	Class   depClass
	Use     string // what needs it, shown when it is missing
	Version string // version flag when not --version
	Brew    string // Homebrew formula, macOS or Linux
	Go      string // module@version, go install into ~/.local/bin
	Npm     string // package@version, npm install --prefix ~/.local
	Py      string // package==version, uv tool install, then pipx
	Mac     string // macOS hint for a report-only row
	Apt     string // Debian package, printed only (apt needs sudo)
	Manual  string // hint on every platform for a report-only row
}

// deps is the host-tool table install --tools and doctor share; node precedes prettier so its npm route can follow.
var deps = []dep{
	{Name: "git", Class: depRequired, Use: "worktrees, memory, review, gates", Mac: "xcode-select --install", Apt: "git"},
	{Name: "go", Class: depRequired, Use: "make install builds the binary", Version: "version", Manual: "make toolchain"},
	{Name: "make", Class: depRequired, Use: "make install, lint, audit", Mac: "xcode-select --install", Apt: "make"},
	{Name: "curl", Class: depRequired, Use: "make rtk downloads the pinned rtk", Mac: "xcode-select --install", Apt: "curl"},
	{Name: "shfmt", Class: depFeature, Use: "format-stop on .sh files", Brew: "shfmt",
		Go: "mvdan.cc/sh/v3/cmd/shfmt@v3.14.1", Apt: "shfmt"},
	{Name: "ruff", Class: depFeature, Use: "format-stop on .py files", Brew: "ruff", Py: "ruff==0.17.5"},
	{Name: "node", Class: depFeature, Use: "workflow lint, browser MCP via npx", Brew: "node", Apt: "nodejs"},
	{Name: "prettier", Class: depFeature, Use: "format-stop on JS/TS/JSON/MD/YAML", Brew: "prettier",
		Npm: "prettier@3.10.1"},
	{Name: "gh", Class: depFeature, Use: "usage quotas, copilot auth switch", Brew: "gh",
		Go: "github.com/cli/cli/v2/cmd/gh@v2.104.0", Apt: "gh"},
	{Name: "tmux", Class: depFeature, Use: "network remote sessions", Version: "-V", Brew: "tmux", Apt: "tmux"},
	{Name: "shellcheck", Class: depGate, Use: "make lint (shell)", Brew: "shellcheck", Apt: "shellcheck"},
	{Name: "golangci-lint", Class: depGate, Use: "make lint (Go)", Manual: "make install DEV_TOOLS=1"},
	{Name: "govulncheck", Class: depGate, Use: "make audit", Manual: "make install DEV_TOOLS=1"},
	{Name: "yamlfmt", Class: depGate, Use: "make lint (YAML)", Manual: "make install DEV_TOOLS=1"},
}

// depTools are deps as install-table rows, so --tools=<name> selects them.
func depTools() []tool {
	out := make([]tool, len(deps))
	for i := range deps {
		out[i] = tool{Name: deps[i].Name, Binary: deps[i].Name, dep: &deps[i]}
	}
	return out
}

// depLookPath and depRun are the seams: tests stub PATH lookups and every installer command.
var (
	depLookPath = paths.LookPathReal
	depRun      = runLogged
)

// route is one installer command; bin must be on PATH for the route to run.
type route struct {
	bin  string
	args []string
	env  []string
}

func (r route) String() string {
	return strings.Join(append(append(append([]string(nil), r.env...), r.bin), r.args...), " ")
}

// depRoutes lists d's no-root installers in preference order; Windows has none (report only).
func depRoutes(d dep) []route {
	if goosOverride == "windows" {
		return nil
	}
	var rs []route
	if d.Brew != "" {
		rs = append(rs, route{bin: "brew", args: []string{"install", d.Brew}})
	}
	local := localBinDir()
	if d.Go != "" && local != "" {
		rs = append(rs, route{bin: "go", args: []string{"install", d.Go}, env: []string{"GOBIN=" + local}})
	}
	if d.Npm != "" && local != "" {
		rs = append(rs, route{bin: "npm", args: []string{"install", "--global", "--prefix", filepath.Dir(local),
			"--no-fund", "--no-audit", d.Npm}})
	}
	if d.Py != "" {
		rs = append(rs, route{bin: "uv", args: []string{"tool", "install", d.Py}}, route{bin: "pipx", args: []string{"install", d.Py}})
	}
	return rs
}

// pickRoute is the first route whose installer is on PATH, with that installer's resolved path.
func pickRoute(rs []route) (route, string, bool) {
	for _, r := range rs {
		if p, err := depLookPath(r.bin); err == nil {
			return r, p, true
		}
	}
	return route{}, "", false
}

// depPath is d's executable on PATH, or "".
func depPath(d dep) string {
	p, err := depLookPath(d.Name)
	if err != nil {
		return ""
	}
	return p
}

// depFix is the one command that installs d here: a runnable route, else the platform's printed hint.
func depFix(d dep) string {
	rs := depRoutes(d)
	if r, _, ok := pickRoute(rs); ok {
		return r.String()
	}
	_, aptErr := depLookPath("apt-get")
	switch {
	case d.Manual != "":
		return d.Manual
	case goosOverride == "darwin" && d.Mac != "":
		return d.Mac
	case goosOverride == "linux" && d.Apt != "" && aptErr == nil:
		return "sudo apt-get install -y " + d.Apt
	case len(rs) > 0:
		return rs[0].String()
	}
	return "install " + d.Name + " and put it on PATH"
}

// depSkipReason says why no route can run for d.
func depSkipReason(d dep) string {
	rs := depRoutes(d)
	switch {
	case goosOverride == "windows":
		return "Windows reports only"
	case len(rs) == 0:
		return "report only (" + string(d.Class) + ")"
	}
	bins := make([]string, len(rs))
	for i, r := range rs {
		bins[i] = r.bin
	}
	return "none of " + strings.Join(bins, ", ") + " on PATH"
}

// installDep prints one line for d: ok, installed or skipped; a failure never fails the install.
func installDep(ctx context.Context, w io.Writer, d dep, dryRun bool) {
	if p := depPath(d); p != "" {
		depLine(w, d, "ok "+p)
		return
	}
	r, bin, ok := pickRoute(depRoutes(d))
	switch {
	case !ok:
		depLine(w, d, "skipped: "+depSkipReason(d)+"; run: "+depFix(d))
		return
	case dryRun:
		depLine(w, d, "would install: "+r.String())
		return
	}
	var out bytes.Buffer
	if err := depRun(ctx, &out, r.env, bin, r.args...); err != nil {
		depLine(w, d, fmt.Sprintf("skipped: %v%s; run: %s", err, lastLine(out.String()), r.String()))
		return
	}
	if depPath(d) == "" {
		depLine(w, d, "installed: "+r.String()+" (not on PATH yet; add its bin dir to PATH)")
		return
	}
	depLine(w, d, "installed: "+r.String())
}

// lastLine is the installer's last output line as " (line)", or "".
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return " (" + last + ")"
	}
	return ""
}

func depLine(w io.Writer, d dep, msg string) { fmt.Fprintf(w, "  %-14s %s\n", d.Name, msg) }

// DepStatus is one host tool's state for doctor: Path is "" when missing, Fix the command that installs it.
type DepStatus struct {
	Name, Class, Use, Path, Version, Fix string
	Required                             bool
}

// DepStatusAll reports every host tool in table order.
func DepStatusAll() []DepStatus {
	out := make([]DepStatus, len(deps))
	for i, d := range deps {
		s := DepStatus{Name: d.Name, Class: string(d.Class), Use: d.Use, Required: d.Class == depRequired}
		if s.Path = depPath(d); s.Path == "" {
			s.Fix = depFix(d)
		} else {
			s.Version = depVersion(d, s.Path)
		}
		out[i] = s
	}
	return out
}

// depVersion is the version in d's probe output at path (else its first line), cached like agent versions.
func depVersion(d dep, path string) string {
	arg := d.Version
	if arg == "" {
		arg = "--version"
	}
	line := ProbedVersionLine(path, func(p string) string {
		return firstOutputLine(context.Background(), p, versionProbeTimeout, arg)
	})
	return cmp.Or(parseVersion(line), line)
}

// depHeader prints the host-tools section header once; printed reports whether it already was.
func depHeader(w io.Writer, printed bool) bool {
	if !printed {
		fmt.Fprintln(w, "== host tools ==")
	}
	return true
}
