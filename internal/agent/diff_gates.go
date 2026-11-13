// Go gates for `agents diff --check`: gofmt, build, vet, lint, complexity, test and docs on the touched packages.

package agent

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/review/source"
)

const (
	diffHeadLines   = 30
	diffGateTimeout = 10 * time.Minute
)

// Seams: production runs real processes; tests swap them so no go or lint process is spawned.
var (
	gateRun      = runGateCmd
	gateLookPath = exec.LookPath
	gateSimplify = source.RunSimplify
	gateBlobs    = gitBlobs
)

// gateInput carries what the gates share: the repo root, the Go packages and the files to check.
type gateInput struct {
	root, base          string
	pkgs, goFiles, srcs []string
	mds                 []string          // changed Markdown files still present
	old                 map[string]string // checked path -> its path at base; absent for a file new since base
}

// gateFn returns the gate's output and a non-empty skip reason when it did not apply; an error is a FAIL.
type gateFn func(ctx context.Context, in gateInput) (out, skip string, err error)

type gateDef struct {
	name       string
	needsBuild bool
	fn         gateFn
}

func newGateInput(root, base string, files []DiffFile) gateInput {
	in := gateInput{root: root, base: base, old: map[string]string{}}
	pkgSet := map[string]bool{}
	for _, f := range files {
		addGateFile(&in, pkgSet, f.Path, f.Status == "D")
		addGateFile(&in, pkgSet, f.From, true)
		if f.Status != "A" && f.Status != "?" {
			in.old[f.Path] = cmp.Or(f.From, f.Path)
		}
	}
	in.pkgs = slices.Sorted(maps.Keys(pkgSet))
	sort.Strings(in.goFiles)
	sort.Strings(in.srcs)
	sort.Strings(in.mds)
	return in
}

// addGateFile records p's package and, unless the file is gone, its place in the gofmt and complexity lists.
func addGateFile(in *gateInput, pkgSet map[string]bool, p string, gone bool) {
	if p == "" || slices.Contains(strings.Split(p, "/"), "testdata") {
		return
	}
	isGo := strings.HasSuffix(p, ".go")
	if dir := path.Dir(p); isGo && dirHasGo(filepath.Join(in.root, filepath.FromSlash(dir))) {
		pkgSet["./"+dir] = true
		if dir == "." {
			pkgSet["."] = true
			delete(pkgSet, "./.")
		}
	}
	if gone {
		return
	}
	if strings.HasSuffix(p, ".md") {
		in.mds = append(in.mds, p)
	}
	if isGo {
		in.goFiles = append(in.goFiles, p)
	}
	if source.DetectLanguage(p) != "" {
		in.srcs = append(in.srcs, p)
	}
}

func dirHasGo(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	return slices.ContainsFunc(ents, func(e os.DirEntry) bool { return strings.HasSuffix(e.Name(), ".go") })
}

func runGates(ctx context.Context, root, base string, files []DiffFile) []GateResult {
	in := newGateInput(root, base, files)
	defs := gateDefs()
	if len(in.pkgs) == 0 {
		out := []GateResult{{Name: "go gates", Status: "skip", Reason: "no Go changes; run make lint for docs"}}
		if len(in.srcs) > 0 {
			out = append(out, runGate(ctx, defs[complexityIdx], in))
		}
		return appendDocs(ctx, out, in)
	}
	res := make([]GateResult, len(defs))
	for i, d := range defs {
		if d.needsBuild && res[buildIdx].Status == "FAIL" {
			res[i] = GateResult{Name: d.name, Status: "skip", Reason: "build failed"}
			continue
		}
		res[i] = runGate(ctx, d, in)
	}
	return appendDocs(ctx, res, in)
}

// appendDocs adds the docs gate after the others, only when the change set holds Markdown.
func appendDocs(ctx context.Context, res []GateResult, in gateInput) []GateResult {
	if len(in.mds) == 0 {
		return res
	}
	return append(res, runGate(ctx, gateDef{name: "docs", fn: docsGate}, in))
}

// Indexes into gateDefs.
const (
	buildIdx      = 1
	complexityIdx = 4
)

// gateDefs lists the gates in report order, run one after another: concurrent vet, lint and test were not faster.
func gateDefs() []gateDef {
	return []gateDef{
		{"gofmt", false, gofmtGate},
		{"build", false, func(ctx context.Context, in gateInput) (string, string, error) {
			out, err := gateRun(ctx, in.root, "go", "build", "./...")
			return out, "", err
		}},
		{"vet", true, pkgGate("go", "vet")},
		{"golangci-lint", true, lintGate},
		{"complexity", false, complexityGate},
		{"test", true, pkgGate("go", "test", "-count=1")},
	}
}

func runGate(ctx context.Context, d gateDef, in gateInput) GateResult {
	start := time.Now()
	out, skip, err := d.fn(ctx, in)
	r := GateResult{Name: d.name, Status: "ok", Seconds: time.Since(start).Round(10 * time.Millisecond).Seconds()}
	switch {
	case skip != "":
		r.Status, r.Reason, r.Seconds = "skip", skip, 0
	case err != nil:
		r.Status = "FAIL"
		r.OutputHead, r.CutLines = cutHead(scrubPaths(out+errText(out, err), in.root), diffHeadLines)
	}
	return r
}

// errText names the failure when the tool printed nothing (a missing binary, a timeout).
func errText(out string, err error) string {
	if strings.TrimSpace(out) != "" {
		return ""
	}
	return err.Error()
}

// scrubPaths drops the repo root and shortens the temp dir and home to <tmp> and ~, so two runs print the same bytes.
func scrubPaths(s, root string) string {
	s = replaceDir(s, root, "")
	home, _ := os.UserHomeDir()
	s = replaceDir(s, os.TempDir(), "<tmp>/")
	return replaceDir(s, home, "~/")
}

// replaceDir swaps dir and its symlink-resolved form, each followed by a slash, for repl; "" and "/" are left alone.
func replaceDir(s, dir, repl string) string {
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		return s
	}
	s = strings.ReplaceAll(s, dir+"/", repl)
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		s = strings.ReplaceAll(s, real+"/", repl)
	}
	return s
}

func cutHead(out string, limit int) (string, int) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) <= limit {
		return strings.Join(lines, "\n"), 0
	}
	return strings.Join(lines[:limit], "\n"), len(lines) - limit
}

func gofmtGate(ctx context.Context, in gateInput) (string, string, error) {
	if len(in.goFiles) == 0 {
		return "", "no Go files", nil
	}
	out, err := gateRun(ctx, in.root, "gofmt", append([]string{"-l"}, in.goFiles...)...)
	if err == nil && strings.TrimSpace(out) != "" {
		err = errors.New("files need gofmt")
	}
	return out, "", err
}

func pkgGate(name string, args ...string) gateFn {
	return func(ctx context.Context, in gateInput) (string, string, error) {
		if len(in.pkgs) == 0 {
			return "", "no Go packages", nil
		}
		out, err := gateRun(ctx, in.root, name, append(args, in.pkgs...)...)
		return out, "", err
	}
}

func lintGate(ctx context.Context, in gateInput) (string, string, error) {
	if _, err := gateLookPath("golangci-lint"); err != nil {
		return "", "not installed", nil
	}
	return pkgGate("golangci-lint", "run")(ctx, in)
}

// docsGate runs the repo's mdsmith (the `make lint-docs` check, same .mdsmith.yml) on the changed Markdown only.
func docsGate(ctx context.Context, in gateInput) (string, string, error) {
	bin := filepath.Join(in.root, "bin", "mdsmith")
	if _, err := gateLookPath(bin); err != nil {
		bin = "mdsmith"
		if _, err := gateLookPath(bin); err != nil {
			return "", "mdsmith not installed", nil
		}
	}
	out, err := gateRun(ctx, in.root, bin, append([]string{"check", "--no-color"}, in.mds...)...)
	return out, "", err
}

// complexityGate runs the `review simplify` engine in-process; only MEDIUM+ findings the change introduced fail it.
func complexityGate(ctx context.Context, in gateInput) (string, string, error) {
	if len(in.srcs) == 0 {
		return "", "no source files", nil
	}
	rep, stderr, err := runSimplify(in.root, in.srcs)
	if err != nil {
		return stderr, "", err
	}
	fresh, note := rep.findings(in.root), ""
	if len(fresh) > 0 {
		fresh, note = introduced(ctx, in, fresh)
	}
	lines := make([]string, 0, len(fresh)+len(rep.Failed)+1)
	if note != "" && len(fresh) > 0 {
		lines = append(lines, note)
	}
	for _, f := range fresh {
		lines = append(lines, f.line)
	}
	for _, f := range rep.Failed {
		lines = append(lines, f+" analysis failed")
	}
	if len(lines) == 0 {
		return "", "", nil
	}
	return strings.Join(lines, "\n"), "", errors.New("findings")
}

// runSimplify runs the detector on rels under dir at the medium threshold.
func runSimplify(dir string, rels []string) (rep simplifyReport, stderr string, err error) {
	args := []string{"--json", "--threshold", "medium"}
	for _, s := range rels {
		args = append(args, filepath.Join(dir, filepath.FromSlash(s)))
	}
	var out, errBuf bytes.Buffer
	code := gateSimplify(args, &out, &errBuf)
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		return rep, errBuf.String(), fmt.Errorf("review simplify exit %d: %v", code, err)
	}
	return rep, "", nil
}

// simplifyReport is the part of `review simplify --json` the gate reads.
type simplifyReport struct {
	Files []struct {
		File       string           `json:"file"`
		Complexity []source.Finding `json:"complexity"`
		Quality    struct {
			Smells []source.Smell `json:"smells"`
		} `json:"quality"`
	} `json:"files"`
	Failed []string `json:"analysis_failed"`
}

// runGateCmd runs one gate process in dir with its combined output; list args only, never a shell.
func runGateCmd(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.WaitDelay = proc.ExecWaitDelay
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}
