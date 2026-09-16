package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/transcript"
)

// Bounds so a large worktree cannot stall Stop past its 30s timeout.
const (
	stopFormatMaxFiles = 50
	stopFormatMaxSize  = 1 << 20
	stopFormatBudget   = 25 * time.Second
	stopFormatWorkers  = 4
)

// StopFormat formats files that are dirty in git AND that this session's transcript shows it writing;
// a dirty file Claude never wrote is the user's hand edit and is left alone.
func StopFormat(r io.Reader) int {
	in := ParseInput(r)
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if cwd == "" {
		return ExitProceed
	}
	stampPath := filepath.Join(paths.StateDir(), "stop-format", cwdKey(cwd)+".json")
	changed := changedFiles(cwd)
	if len(changed) == 0 {
		_ = os.Remove(stampPath) // clean tree: drop stale stamps and skip the transcript scan
		return ExitProceed
	}
	allow := map[string]bool{}
	for _, f := range transcript.EditedFiles(in.TranscriptPath, stopFormatMaxFiles) {
		if abs, err := filepath.Abs(f); err == nil {
			allow[abs] = true
		}
	}
	if len(allow) == 0 {
		return ExitProceed
	}
	eligible, carry := selectForFormat(cwd, changed, loadStamps(stampPath), allow)
	for file, key := range formatAll(eligible) {
		carry[file] = key
	}
	if data, err := json.Marshal(carry); err == nil {
		_ = paths.AtomicWrite(stampPath, data, 0o600)
	}
	return ExitProceed
}

// selectForFormat returns dirty session-written files whose stamp changed, plus stamps to carry forward.
func selectForFormat(cwd string, changed []string, prev map[string]string, allow map[string]bool) ([]string, map[string]string) {
	var eligible []string
	carry := map[string]string{}
	for _, rel := range changed {
		full := filepath.Join(cwd, rel)
		if !allow[full] {
			continue
		}
		fi, err := os.Stat(full)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > stopFormatMaxSize {
			continue
		}
		if key := stampKey(fi); prev[full] == key {
			carry[full] = key
			continue
		}
		if len(eligible) < stopFormatMaxFiles {
			eligible = append(eligible, full)
		}
	}
	return eligible, carry
}

// formatAll formats files concurrently under the budget; files still running at the deadline stay unstamped.
func formatAll(files []string) map[string]string {
	var mu sync.Mutex
	out := map[string]string{}
	sem := make(chan struct{}, stopFormatWorkers)
	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, f := range files {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if !formatFile(f) {
				return
			}
			if fi, err := os.Stat(f); err == nil {
				mu.Lock()
				out[f] = stampKey(fi)
				mu.Unlock()
			}
		}()
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(stopFormatBudget):
	}
	mu.Lock()
	defer mu.Unlock()
	return maps.Clone(out)
}

// stampKey is mtime+size: cheaper than a hash and enough to skip a no-op re-format.
func stampKey(fi os.FileInfo) string {
	return strconv.FormatInt(fi.ModTime().UnixNano(), 10) + ":" + strconv.FormatInt(fi.Size(), 10)
}

func cwdKey(cwd string) string {
	sum := sha256.Sum256([]byte(cwd))
	return hex.EncodeToString(sum[:8])
}

func loadStamps(path string) map[string]string {
	m := map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &m)
	}
	if m == nil {
		m = map[string]string{}
	}
	return m
}

// changedFiles lists modified, staged and untracked paths relative to cwd; -z avoids git's C-quoting.
func changedFiles(cwd string) []string {
	var out []string
	seen := map[string]bool{}
	for _, args := range [][]string{
		{"diff", "--name-only", "--relative", "-z"},
		{"diff", "--name-only", "--cached", "--relative", "-z"},
		{"ls-files", "--others", "--exclude-standard", "-z"},
	} {
		b, err := exec.Command("git", append([]string{"-C", cwd}, args...)...).Output()
		if err != nil {
			continue
		}
		for _, p := range strings.Split(string(b), "\x00") {
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}
