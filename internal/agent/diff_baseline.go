// The complexity gate's baseline: the detector's findings for a file at --base, so only what a change introduced fails.

package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
)

// fileFinding is one finding with what identifies it across versions of a file: kind, function name and value.
type fileFinding struct {
	file, kind, name string // file is relative to the dir the detector ran on
	value            int
	line             string
}

func (f fileFinding) id() string { return f.file + "|" + f.kind + "|" + f.name }

var (
	quotedName = regexp.MustCompile(`'([^']*)'`)
	firstInt   = regexp.MustCompile(`\d+`)
)

// newFinding reads the function name (quoted in the message, else location) and the first number of msg.
func newFinding(file, kind, location, msg, line string) fileFinding {
	name := location
	if m := quotedName.FindStringSubmatch(msg); m != nil {
		name = m[1]
	}
	value, _ := strconv.Atoi(firstInt.FindString(quotedName.ReplaceAllString(msg, "")))
	return fileFinding{file: file, kind: kind, name: name, value: value, line: line}
}

// findings lists complexity findings and smells rated medium or above; dir is stripped to key each file.
func (rep simplifyReport) findings(dir string) []fileFinding {
	var out []fileFinding
	for _, f := range rep.Files {
		rel := strings.TrimPrefix(f.File, dir+"/")
		for _, c := range f.Complexity {
			line := fmt.Sprintf("%s line %d [%s] %s: %s", f.File, c.Line, c.Severity, c.Rule, c.Message)
			out = append(out, newFinding(rel, c.Rule, "", c.Message, line))
		}
		for _, s := range f.Quality.Smells {
			if s.Severity == "medium" || s.Severity == "high" || s.Severity == "critical" {
				line := fmt.Sprintf("%s %s [%s] %s: %s", f.File, s.Location, s.Severity, s.Type, s.Message)
				out = append(out, newFinding(rel, s.Type, s.Location, s.Message, line))
			}
		}
	}
	return out
}

// introduced drops findings the base already had (same file, kind, function, value); note says why a base read failed.
func introduced(ctx context.Context, in gateInput, after []fileFinding) (fresh []fileFinding, note string) {
	before, err := baselineFindings(ctx, in, after)
	if err != nil {
		return after, "baseline unavailable, every finding counts: " + err.Error()
	}
	return dropOld(after, before), ""
}

// baselineFindings runs the detector on the base version of each file in after that existed at the base.
func baselineFindings(ctx context.Context, in gateInput, after []fileFinding) ([]fileFinding, error) {
	var want []string
	for _, f := range after {
		if old := in.old[f.file]; old != "" && !slices.Contains(want, old) {
			want = append(want, old)
		}
	}
	if len(want) == 0 {
		return nil, nil
	}
	blobs, err := gateBlobs(ctx, in.root, in.base, want)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "agents-diff-base-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	rels, err := writeBaseline(dir, in.old, blobs, after)
	if err != nil || len(rels) == 0 {
		return nil, err
	}
	rep, _, err := runSimplify(dir, rels)
	return rep.findings(dir), err
}

// writeBaseline writes each after file's base blob under dir at the file's current path and returns those paths.
func writeBaseline(dir string, old, blobs map[string]string, after []fileFinding) ([]string, error) {
	var rels []string
	for _, f := range after {
		body, ok := blobs[old[f.file]]
		if !ok || slices.Contains(rels, f.file) {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(f.file))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			return nil, err
		}
		rels = append(rels, f.file)
	}
	return rels, nil
}

// dropOld returns the after findings no before finding covers: same file, kind and function, with at least the value.
func dropOld(after, before []fileFinding) []fileFinding {
	old := map[string][]int{}
	for _, b := range before {
		old[b.id()] = append(old[b.id()], b.value)
	}
	for _, vals := range old {
		slices.Sort(vals)
	}
	var out []fileFinding
	for _, a := range after {
		vals := old[a.id()]
		if i := slices.IndexFunc(vals, func(v int) bool { return v >= a.value }); i >= 0 {
			old[a.id()] = slices.Delete(vals, i, i+1)
			continue
		}
		out = append(out, a)
	}
	return out
}

// gitBlobs reads every path at rev with one `git cat-file --batch`; paths absent at rev are left out of the result.
func gitBlobs(ctx context.Context, root, rev string, rels []string) (map[string]string, error) {
	var req strings.Builder
	var sent []string
	for _, p := range rels {
		if !strings.ContainsAny(p, "\n") {
			fmt.Fprintf(&req, "%s:%s\n", rev, p)
			sent = append(sent, p)
		}
	}
	cmd := exec.CommandContext(ctx, "git", "-C", root, "cat-file", "--batch")
	cmd.Env, cmd.WaitDelay, cmd.Stdin = proc.GitEnv(), proc.ExecWaitDelay, strings.NewReader(req.String())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return readBlobs(out, sent)
}

// readBlobs parses cat-file --batch output: per path `<oid> blob <size>` and the content, or `<spec> missing`.
func readBlobs(out []byte, sent []string) (map[string]string, error) {
	r, blobs := bufio.NewReader(bytes.NewReader(out)), map[string]string{}
	for _, p := range sent {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("git cat-file: short output: %w", err)
		}
		f := strings.Fields(hdr)
		if len(f) != 3 || f[1] != "blob" {
			continue
		}
		size, _ := strconv.Atoi(f[2])
		body := make([]byte, size+1) // the blob and git's trailing newline
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, fmt.Errorf("git cat-file: short blob: %w", err)
		}
		blobs[p] = string(body[:size])
	}
	return blobs, nil
}

// binarySniffBytes is how much of a file's head is scanned for a NUL, as git does.
const binarySniffBytes = 8000

// countFileLines counts the lines of an untracked regular file; a NUL in the first block marks it binary, as git does.
func countFileLines(p string) (lines int, binary bool) {
	if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() {
		return 0, false
	}
	f, err := os.Open(p)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	buf := make([]byte, 64<<10)
	var last byte = '\n'
	for first := true; ; first = false {
		n, err := r.Read(buf)
		if first && bytes.IndexByte(buf[:min(n, binarySniffBytes)], 0) >= 0 {
			return 0, true
		}
		if n > 0 {
			lines += bytes.Count(buf[:n], []byte{'\n'})
			last = buf[n-1]
		}
		if err != nil {
			break
		}
	}
	if last != '\n' {
		lines++
	}
	return lines, false
}
