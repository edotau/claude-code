package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/ui/helptext"
)

const findFileLimit = 1 << 20
const findWorkers = 8
const findSniffBytes = 8 << 10

// FindDefaultMax is the default result-line budget.
const FindDefaultMax = 200

// FindOptions configures a search over tracked and unignored files from any directory inside a repository.
type FindOptions struct {
	Dir, Pattern                                            string
	Paths                                                   []string
	Name, Func, Def, Callers, IgnoreCase, Word, Files, JSON bool
	Max                                                     int
	Out, Err                                                io.Writer
}

// FindMatch is one matching source line or path.
type FindMatch struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
	Text string `json:"text,omitempty"`
}

// FindReport uses More as a lower bound because the search stops at the first omitted result.
type FindReport struct {
	Matches []FindMatch `json:"matches"`
	More    int         `json:"more"`
}

// Find returns 0 for matches, 1 for no matches, and 2 for invalid input or search errors.
func Find(o FindOptions) int {
	rep, err := findReport(o)
	if err == nil {
		err = renderFind(o, rep)
	}
	if err != nil {
		fmt.Fprintf(o.Err, "agents find: %v\n", err)
		return 2
	}
	if len(rep.Matches) == 0 {
		return 1
	}
	return 0
}

func findRegexp(o FindOptions) (*regexp.Regexp, error) {
	modes := 0
	for _, enabled := range []bool{o.Name, o.Func, o.Def, o.Callers} {
		if enabled {
			modes++
		}
	}
	if modes > 1 {
		return nil, errors.New("choose only one of --name, --func, --def, --callers")
	}
	if o.Max < 1 {
		return nil, errors.New("--max must be positive")
	}
	pattern := o.Pattern
	if o.Callers {
		pattern = "^(?:" + regexp.QuoteMeta(pattern) + ")$"
	}
	if o.Word {
		pattern = `\b(?:` + pattern + `)\b`
	}
	if o.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("pattern: %w", err)
	}
	return re, nil
}

func findReport(o FindOptions) (FindReport, error) {
	rep := FindReport{Matches: []FindMatch{}}
	re, err := findRegexp(o)
	if err != nil {
		return rep, err
	}
	root, err := repoRoot(o.Dir)
	if err != nil {
		return rep, err
	}
	files, err := findTracked(root, o)
	if err != nil {
		return rep, err
	}
	for start := 0; start < len(files); start += findWorkers {
		batch := files[start:min(start+findWorkers, len(files))]
		rows, errs := findBatch(root, batch, o, re)
		for i, matches := range rows {
			if errs[i] != nil {
				return rep, fmt.Errorf("%s: %w", batch[i], errs[i])
			}
			for _, match := range matches {
				if len(rep.Matches) == o.Max {
					rep.More = 1
					return rep, nil
				}
				rep.Matches = append(rep.Matches, match)
			}
		}
	}
	return rep, nil
}

func findTracked(root string, o FindOptions) ([]string, error) {
	sel, err := cleanPaths(root, o.Paths)
	if err != nil {
		return nil, err
	}
	raw, err := proc.RunGitRaw(context.Background(), root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	files := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	slices.Sort(files)
	files = slices.Compact(files)
	files = slices.DeleteFunc(files, func(p string) bool {
		return p == "" || (len(sel) > 0 && !inPaths(sel, p)) ||
			((o.Func || o.Def || o.Callers) && !strings.HasSuffix(p, ".go"))
	})
	return files, nil
}

func findBatch(root string, files []string, o FindOptions, re *regexp.Regexp) ([][]FindMatch, []error) {
	rows, errs := make([][]FindMatch, len(files)), make([]error, len(files))
	var wg sync.WaitGroup
	for i, p := range files {
		wg.Go(func() { rows[i], errs[i] = findFile(root, p, o, re) })
	}
	wg.Wait()
	return rows, errs
}

func findFile(root, p string, o FindOptions, re *regexp.Regexp) ([]FindMatch, error) {
	data, err := readFindTracked(root, p)
	if err != nil || data == nil {
		return nil, err
	}
	if o.Name {
		if re.MatchString(p) {
			return []FindMatch{{Path: p}}, nil
		}
		return nil, nil
	}
	if o.Func || o.Def || o.Callers {
		return findGo(p, data, o, re)
	}
	return findContent(p, data, o, re), nil
}

func findContent(p string, data []byte, o FindOptions, re *regexp.Regexp) []FindMatch {
	matches := []FindMatch{}
	content := bytes.Split(data, []byte{'\n'})
	for i, line := range content {
		if i == len(content)-1 && len(line) == 0 {
			break
		}
		if !re.Match(line) {
			continue
		}
		matches = append(matches, FindMatch{Path: p, Line: i + 1, Text: string(line)})
		if o.Files {
			return []FindMatch{{Path: p}}
		}
		if len(matches) > o.Max {
			break
		}
	}
	return matches
}

func readFindTracked(root, p string) ([]byte, error) {
	full := filepath.Join(root, filepath.FromSlash(p))
	st, err := os.Lstat(full)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > findFileLimit {
		return nil, nil
	}
	return readFindFile(full)
}

func readFindFile(full string) ([]byte, error) {
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, findFileLimit+1))
	closeErr := f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > findFileLimit || bytes.IndexByte(data[:min(len(data), findSniffBytes)], 0) >= 0 {
		return nil, nil
	}
	return data, nil
}

func renderFind(o FindOptions, rep FindReport) error {
	if o.JSON {
		return helptext.WriteJSON(o.Out, rep)
	}
	for _, m := range rep.Matches {
		var err error
		if m.Line == 0 {
			_, err = fmt.Fprintln(o.Out, m.Path)
		} else {
			_, err = fmt.Fprintf(o.Out, "%s:%d:%s\n", m.Path, m.Line, m.Text)
		}
		if err != nil {
			return err
		}
	}
	if rep.More > 0 {
		_, err := fmt.Fprintf(o.Out, "... %d more (raise --max or narrow --path)\n", rep.More)
		return err
	}
	return nil
}
