package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/dev/transcript"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/schemas"
)

const (
	catchupWindow = 14 * 24 * time.Hour // transcripts older than this are never swept
	catchupMax    = 3                   // sessions saved per sweep, newest first
	catchupRetry  = 24 * time.Hour      // gap before a failed catch-up gets its second retry
)

// sweepCand is one finished session the sweep would save.
type sweepCand struct {
	id, path, why string
	edits         int
	size          int64
	mtime         time.Time
}

const headCacheSchema = 1

// headVerdict is transcriptInteractive's answer for the file as sized and stamped here.
type headVerdict struct {
	Size        int64 `json:"size"`
	MtimeNS     int64 `json:"mtime_ns"`
	Interactive bool  `json:"interactive"`
}

// headCache is state/harness/memory/catchup-heads.json: transcript path -> verdict, so unchanged heads are not rescanned.
type headCache struct {
	Schema int                    `json:"schema"`
	Files  map[string]headVerdict `json:"files"`
	dirty  bool
}

var headCacheFormat = schemas.Register(schemas.Format{
	Name: "memory-catchup-heads", Owner: "internal/memory", Kind: schemas.Cache,
	Glob:    func() string { return headCachePath() },
	New:     func() any { return &headCache{} },
	Version: headCacheSchema, Keep: "rebuildable cache; a corrupt or newer file is a miss",
})

func headCachePath() string { return filepath.Join(paths.MemoryStateDir(), "catchup-heads.json") }

// loadHeadCache reads the cache file; a missing, corrupt or newer one yields an empty cache.
func loadHeadCache() *headCache {
	c := &headCache{}
	if raw, err := os.ReadFile(headCachePath()); err != nil || !schemas.DecodeOK(headCacheFormat, raw, c) {
		c = &headCache{}
	}
	if c.Files == nil {
		c.Files = map[string]headVerdict{}
	}
	c.Schema = headCacheSchema
	return c
}

// interactive is the cached verdict while size and mtime still match, else a fresh head scan; never trusts a changed file.
func (h *headCache) interactive(c sweepCand) bool {
	if v, ok := h.Files[c.path]; ok && v.Size == c.size && v.MtimeNS == c.mtime.UnixNano() {
		return v.Interactive
	}
	got := transcriptInteractive(c.path)
	h.Files[c.path] = headVerdict{Size: c.size, MtimeNS: c.mtime.UnixNano(), Interactive: got}
	h.dirty = true
	return got
}

// save drops entries outside the window and writes the file when anything changed; best effort.
func (h *headCache) save(recent []sweepCand) {
	live := make(map[string]bool, len(recent))
	for _, c := range recent {
		live[c.path] = true
	}
	for p := range h.Files {
		if !live[p] {
			delete(h.Files, p)
			h.dirty = true
		}
	}
	if !h.dirty {
		return
	}
	if data, err := json.Marshal(h); err == nil && os.MkdirAll(filepath.Dir(headCachePath()), 0o700) == nil {
		_ = paths.AtomicWriteNoSync(headCachePath(), data, 0o600) // rebuilt on the next sweep if torn
	}
}

func catchupDonePath() string { return filepath.Join(paths.MemoryStateDir(), "catchup-done") }

// transcriptInteractive scans through the first user prompt, even after a long metadata prelude.
func transcriptInteractive(path string) (interactive bool) {
	interactive = true // Replaying a worker costs a call; skipping an uncertain session loses its save.
	entrypoint := ""
	scanTranscriptHead(path, math.MaxInt, func(line []byte) bool {
		var rec struct {
			Type       string `json:"type"`
			Entrypoint string `json:"entrypoint"`
			Message    struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil {
			return false
		}

		if rec.Entrypoint != "" {
			entrypoint = rec.Entrypoint
		}
		if rec.Type != "user" { // read on to the first prompt: a memory job inherits its parent's entrypoint
			return false
		}
		interactive = !strings.HasPrefix(entrypoint, "sdk-") && !memoryJobPrompt(transcriptPrompt(rec.Message.Content))
		return true
	})
	return interactive
}

var memoryJobPatterns = sync.OnceValue(func() []*regexp.Regexp {
	return []*regexp.Regexp{
		workerPromptPattern(assets.MemoryUpdateWorker("%s")),
		workerPromptPattern(assets.MemoryDreamWorker("%s")),
	}
})

func workerPromptPattern(format string) *regexp.Regexp {
	return regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(format), "%s", `[^\r\n]+?`))
}

func memoryJobPrompt(s string) bool {
	return slices.ContainsFunc(memoryJobPatterns(), func(re *regexp.Regexp) bool { return re.MatchString(s) })
}

func transcriptPrompt(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var out strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			out.WriteString(b.Text)
		}
	}
	return out.String()
}

// sweepSkip is why a finished session needs no save ("" = it does), plus the receipt it read once for sweepWhy.
func sweepSkip(c sweepCand, current string, now time.Time, heads *headCache) (string, updateResult, bool) {
	switch {
	case c.id == current:
		return "current session", updateResult{}, false
	case now.Sub(c.mtime) < updateRearmCooldown:
		return "live (modified within cooldown)", updateResult{}, false
	case !heads.interactive(c):
		return "headless (worker/leaf/ping)", updateResult{}, false
	}
	res, ok := readUpdateResult(c.id)
	if ok && res.status == updateOK && res.time.After(c.mtime) {
		return "already saved", res, ok
	}
	return "", res, ok
}

// sweepWhy returns why an eligible session needs saving, or "" when its edits are covered; res is c's receipt.
func sweepWhy(c *sweepCand, now time.Time, count func(string, string) int, res updateResult, hasResult bool) string {
	st, hasStamp := readStamp(stampPath(c.id))
	covered := 0
	if hasStamp {
		covered = st.edits
	}
	c.edits = count(c.path, countPath(c.id))
	if hasResult && res.needsAttention(now) && !ownerFixRequired(res) && (!hasStamp || st.state != "catchup" || secondRetryDue(st, res, now)) {
		return "update " + res.status
	}
	if newEditsSinceFailure(*c, covered, res, hasResult) {
		return fmt.Sprintf("%d edits, %d covered", c.edits, covered)
	}
	return ""
}

// newEditsSinceFailure admits a failed owner-fix session only when its transcript changed after the failure.
func newEditsSinceFailure(c sweepCand, covered int, r updateResult, hasResult bool) bool {
	if c.edits < updateMinEdits || c.edits <= covered {
		return false
	}
	return !hasResult || !ownerFixRequired(r) || c.mtime.After(r.time)
}

// ownerFixRequired marks failures that a retry of the same edits cannot repair.
func ownerFixRequired(r updateResult) bool {
	return r.status == updateFailed && (r.class == JobClassAuth || r.class == JobClassBudget)
}

// secondRetryDue: one catch-up ran, its failed receipt is within a day of its stamp, and a day has passed.
func secondRetryDue(st updateStamp, res updateResult, now time.Time) bool {
	return st.attempts < 2 && now.Sub(st.at) > catchupRetry && res.time.Sub(st.at) < catchupRetry
}

// retryStamp is a catch-up stamp's time and attempt count; a failed one keeps its time and counts up.
func retryStamp(id string, now time.Time) (time.Time, int) {
	st, ok := readStamp(stampPath(id))
	res, hasResult := readUpdateResult(id)
	if ok && st.state == "catchup" && hasResult && res.needsAttention(now) {
		return st.at, max(st.attempts, 1) + 1
	}
	return time.Now(), 1
}

// sweepCandidates lists finished sessions needing a save, newest first; all=false stops at catchupMax.
func sweepCandidates(current string, now time.Time, all bool, count func(string, string) int) ([]sweepCand, map[string]int) {
	recent := recentTranscripts(now)
	heads := loadHeadCache()
	defer heads.save(recent)
	var out []sweepCand
	skips := map[string]int{}
	for _, c := range recent {
		if !all && len(out) >= catchupMax {
			break
		}
		skip, res, ok := sweepSkip(c, current, now, heads)
		if skip == "" {
			if c.why = sweepWhy(&c, now, count, res, ok); c.why == "" {
				skip = "nothing new"
				if ok && ownerFixRequired(res) {
					skip = "needs fix (" + res.class + ")"
				}
			}
		}
		if skip != "" {
			skips[skip]++
			continue
		}
		out = append(out, c)
	}
	return out, skips
}

// recentTranscripts lists transcripts modified inside the catch-up window, newest first.
func recentTranscripts(now time.Time) []sweepCand {
	dirs := transcript.GlobIn(paths.ConfigDir(), "projects/*")
	per := make([][]sweepCand, len(dirs))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(8, len(dirs)) { // each project dir's listing and stats are independent: workers take dirs in turn
		wg.Go(func() {
			for d := int(next.Add(1)) - 1; d < len(dirs); d = int(next.Add(1)) - 1 {
				for _, p := range transcript.GlobIn(dirs[d], "*.jsonl") {
					id := strings.TrimSuffix(filepath.Base(p), ".jsonl")
					if !looksLikeSessionID(id) {
						continue
					}
					if fi, err := os.Stat(p); err == nil && now.Sub(fi.ModTime()) <= catchupWindow {
						per[d] = append(per[d], sweepCand{id: id, path: p, size: fi.Size(), mtime: fi.ModTime()})
					}
				}
			}
		})
	}
	wg.Wait()
	recent := slices.Concat(per...)
	sort.Slice(recent, func(i, j int) bool { return recent[i].mtime.After(recent[j].mtime) })
	return recent
}

// SweepOptions are `memory update --catchup`'s flags: DryRun lists without saving, Skip is the session to leave alone.
type SweepOptions struct {
	DryRun         bool
	Skip           string
	Stdout, Stderr io.Writer
}

// CatchUpSweep is `memory update --catchup`: saves up to catchupMax finished sessions whose update was missed.
func CatchUpSweep(o SweepOptions) int {
	stdout, stderr := o.Stdout, o.Stderr
	current := o.Skip
	now := time.Now()
	if o.DryRun {
		cands, skips := sweepCandidates(current, now, true, readOnlyEdits)
		for i, c := range cands {
			verb := "would save"
			if i >= catchupMax {
				verb = "queued (cap)"
			}
			fmt.Fprintf(stdout, "%s %s  %s  (%s)\n", verb, c.id, c.mtime.Format("2006-01-02 15:04"), c.why)
		}
		fmt.Fprintf(stdout, "skipped: %v\n", skips)
		return 0
	}
	if err := os.MkdirAll(paths.MemoryStateDir(), 0o700); err != nil {
		fmt.Fprintf(stderr, "memory update --catchup: %v\n", err)
		return 1
	}
	lock := filepath.Join(paths.MemoryStateDir(), "catchup.lock")
	for pass := range 3 {
		ran, err := paths.TryFileLock(lock, 0, func() error {
			if pass == 0 { // a retry pass only drains: the sweep's saves may each launch a model run
				runSweep(current, now, stdout)
			}
			drainQueue(stdout)
			return nil
		})
		if err != nil {
			fmt.Fprintf(stderr, "memory update --catchup: %v\n", err)
			return 1
		}
		// A busy lock means another sweep is running; a job filed after its drain needs one more pass.
		if !ran || pending() == 0 {
			break
		}
	}
	return 0
}

// runSweep saves each candidate in turn and records how many ended ok for the next SessionStart.
func runSweep(current string, now time.Time, out io.Writer) {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(out, "memory update --catchup:", err)
		return
	}
	cands, _ := sweepCandidates(current, now, false, countEdits)
	saved := 0
	for _, c := range cands {
		// Stamped first, so a crash or failure retries at most twice.
		at, attempts := retryStamp(c.id, now)
		if err := writeStampAttempts(stampPath(c.id), at, "catchup", c.edits, attempts); err != nil {
			fmt.Fprintf(out, "catchup %s: write retry stamp: %v\n", c.id, err)
			continue
		}
		fmt.Fprintf(out, "catchup %s (%s)\n", c.id, c.why)
		ctx, cancel := context.WithTimeout(context.Background(), updateTimeout+2*time.Minute)
		started := time.Now()
		if err := runHistoricalUpdate(ctx, self, c, out); err != nil {
			fmt.Fprintf(out, "catchup %s: %v\n", c.id, err)
		}
		cancel()
		// An older OK receipt from a previous update must not count as this run's save.
		if res, ok := readUpdateResult(c.id); ok && res.status == updateOK && res.time.After(started) {
			saved++
		}
	}
	if saved > 0 {
		prev := catchupDoneCount()
		if err := paths.AtomicWrite(catchupDonePath(), []byte(strconv.Itoa(prev+saved)), 0o600); err != nil {
			fmt.Fprintf(out, "memory update --catchup: %d session(s) saved but the done note failed: %v\n", saved, err)
		}
	}
}

// runHistoricalUpdate is the subprocess boundary; tests never execute their own binary.
var runHistoricalUpdate = func(ctx context.Context, self string, c sweepCand, out io.Writer) error {
	cmd := exec.CommandContext(ctx, self, "memory", "update", "--historical", "--session", c.id, "--transcript", c.path)
	cmd.Stdout, cmd.Stderr = out, out
	proc.OwnGroup(cmd) // the timeout kills the update's whole group, so its claude child cannot outlive the lock
	cmd.WaitDelay = proc.ExecWaitDelay
	return cmd.Run()
}

// catchupDoneCount is the saved-session count in the done note; 0 when absent or unreadable.
func catchupDoneCount() int {
	b, _ := readIfExists(catchupDonePath())
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// ConsumeCatchupNote is the quiet SessionStart note (read once): a finished sweep's saved count, or "" with none.
func ConsumeCatchupNote() string {
	if _, err := os.Stat(catchupDonePath()); err != nil {
		return "" // common case: no note, so skip the lock
	}
	n := 0
	// A running sweep holds the lock and still updates the count: leave it for the next start.
	_, _ = paths.TryFileLock(filepath.Join(paths.MemoryStateDir(), "catchup.lock"), 0, func() error {
		n = catchupDoneCount()
		return os.Remove(catchupDonePath())
	})
	switch {
	case n <= 0:
		return ""
	case n == 1:
		return "memory: saved 1 earlier session"
	}
	return fmt.Sprintf("memory: saved %d earlier sessions", n)
}
