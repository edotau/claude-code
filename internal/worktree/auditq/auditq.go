// Package auditq is the post-land audit worker: one SQLite table, one transaction per change, linked by the CLI only.
package auditq

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/sqlite"
	"github.com/edotau/claude-code/internal/worktree"
)

// auditTimeout bounds one slot wait and one audit run, so a stuck command cannot hold the worker lock forever.
var auditTimeout = worktree.AuditStale

// auditGrace is how long a timed-out audit gets after SIGTERM before its tree is killed.
const auditGrace = 10 * time.Second

type worker struct {
	db   *sql.DB
	lock *os.File // the held worker flock, this process's only: a crash frees it, the slot check fences its audit
}

// lockGrace outlasts a prompt hook's flock probe; a real holder keeps the lock far longer.
const (
	lockGrace = time.Second
	lockTries = 20
)

// Run drains the queue under the one worker flock; a worker that cannot take it exits, the holder imports its lines.
func Run() {
	ctx := context.Background()
	db, err := open(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "audit:", err)
		return
	}
	defer db.Close()
	w := worker{db: db}
	if err = w.holdAndDrain(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "audit:", err)
	}
}

// holdAndDrain takes the worker flock (another holder: nothing to do), prunes and drains; the last claim releases it.
func (w *worker) holdAndDrain(ctx context.Context) error {
	f, err := os.OpenFile(worktree.AuditWorkerLock(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close() // a second close after the last claim's release is harmless
	end := time.Now().Add(lockGrace)
	for tries := 1; !paths.TryLock(f); tries++ {
		if tries >= lockTries && time.Now().After(end) { // a stalled start still gets every try
			return nil
		}
		time.Sleep(lockGrace / lockTries)
	}
	w.lock = f
	if err := w.prune(ctx); err != nil {
		return err
	}
	return w.drain(ctx)
}

// drain claims and runs queued rows until none is left.
func (w *worker) drain(ctx context.Context) error {
	for {
		jobs, err := w.claim(ctx)
		if err != nil || len(jobs) == 0 {
			return err
		}
		if err := w.runJobs(ctx, jobs); errors.Is(err, worktree.ErrSlotBusy) {
			return nil // requeued: a later worker runs them once the slot frees
		} else if err != nil {
			return err
		}
	}
}

// publish runs fn in one transaction with the spool's import, then writes the committed rows to verdicts.json.
func (w *worker) publish(ctx context.Context, fn func(*sql.Tx, int64) error) error {
	return w.publishThen(ctx, fn, nil)
}

// publishThen is publish with then run after it succeeds, still under the spool lock.
func (w *worker) publishThen(ctx context.Context, fn func(*sql.Tx, int64) error, then func()) error {
	spool := worktree.AuditSpool()
	return paths.WithFileLock(spool, func() error {
		items, err := worktree.ReadAuditSpool(spool)
		if err != nil {
			return err
		}
		var rows []worktree.AuditRow
		err = sqlite.Tx(ctx, w.db, func(tx *sql.Tx) error {
			now := time.Now().Unix()
			for _, it := range items { // the token is the key: a replayed line is ignored
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO audits(sha, queued, repo, base, state)
					VALUES(?, ?, ?, ?, 'queued')`, it.SHA, it.Queued, it.Repo, it.Base); err != nil {
					return err
				}
			}
			if err := fn(tx, now); err != nil {
				return err
			}
			var err error
			rows, err = allRows(ctx, tx)
			return err
		})
		if err == nil {
			err = worktree.WriteAuditSnapshot(rows)
		}
		if err != nil { // the spool stays: its import is idempotent
			return err
		}
		if err = os.Remove(spool); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if then != nil {
			then()
		}
		return nil
	})
}

func allRows(ctx context.Context, tx *sql.Tx) ([]worktree.AuditRow, error) {
	q, err := tx.QueryContext(ctx, `SELECT repo, base, sha, queued, state, started, ended, log, with_shas, code, secs
		FROM audits ORDER BY queued`)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	var rows []worktree.AuditRow
	for q.Next() {
		var r worktree.AuditRow
		var with string
		if err := q.Scan(&r.Repo, &r.Base, &r.SHA, &r.Queued, &r.State, &r.Started, &r.Ended, &r.Log, &with, &r.Code, &r.Secs); err != nil {
			return nil, err
		}
		r.With = strings.Fields(with)
		rows = append(rows, r)
	}
	return rows, q.Err()
}

// claim takes every queued row and each dead worker's free one; with none it frees the flock under the spool lock.
func (w *worker) claim(ctx context.Context) (jobs []worktree.AuditItem, err error) {
	release := func() {
		if len(jobs) == 0 {
			_ = w.lock.Close()
		}
	}
	free, err := w.orphansFree(ctx)
	if err != nil {
		return nil, err
	}
	err = w.publishThen(ctx, func(tx *sql.Tx, now int64) error {
		jobs = nil
		for _, it := range free {
			if _, err := tx.ExecContext(ctx, `UPDATE audits SET state = 'queued' WHERE sha = ? AND queued = ? AND state = 'running'`,
				it.SHA, it.Queued); err != nil {
				return err
			}
		}
		q, err := tx.QueryContext(ctx, `SELECT repo, base, sha, queued FROM audits WHERE state = 'queued' ORDER BY queued`)
		if err != nil {
			return err
		}
		for q.Next() {
			var it worktree.AuditItem
			if err := q.Scan(&it.Repo, &it.Base, &it.SHA, &it.Queued); err != nil {
				_ = q.Close()
				return err
			}
			jobs = append(jobs, it)
		}
		if err := errors.Join(q.Err(), q.Close()); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE audits SET state = 'running', started = ?, slot = '', run = '' WHERE state = 'queued'`, now)
		return err
	}, release)
	return jobs, err
}

// orphansFree is the running rows, all a dead worker's under the flock, no live process is in or carries the tag of.
func (w *worker) orphansFree(ctx context.Context) ([]worktree.AuditItem, error) {
	q, err := w.db.QueryContext(ctx, `SELECT sha, queued, slot, run FROM audits WHERE state = 'running'`)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	var free []worktree.AuditItem
	for q.Next() {
		var it worktree.AuditItem
		var slot, run string
		if err := q.Scan(&it.SHA, &it.Queued, &slot, &run); err != nil {
			return nil, err
		}
		if !runBusy(slot, run) {
			free = append(free, it)
		}
	}
	return free, q.Err()
}

// runBusy: a live process is in slot or carries run's tag; unreadable is busy, so a row never runs twice.
func runBusy(slot, run string) bool {
	if slot != "" {
		if busy, err := worktree.InUseBy(slot); err != nil || busy != "" {
			return true
		}
	}
	if run == "" {
		return false
	}
	alive, err := proc.TreeAlive(run)
	return alive || err != nil
}

// update applies stmt (args, then sha and queued) to each item's running row.
func (w *worker) update(ctx context.Context, items []worktree.AuditItem, stmt string, args func(worktree.AuditItem) []any) error {
	return w.publish(ctx, func(tx *sql.Tx, _ int64) error {
		for _, it := range items {
			if _, err := tx.ExecContext(ctx, stmt+` WHERE sha = ? AND queued = ? AND state = 'running'`,
				append(args(it), it.SHA, it.Queued)...); err != nil {
				return err
			}
		}
		return nil
	})
}

// markRun points each item's row at the run now covering it, its slot and its tree tag.
func (w *worker) markRun(ctx context.Context, items []worktree.AuditItem, dir, tag, logPath string, start time.Time) error {
	return w.update(ctx, items, `UPDATE audits SET slot = ?, run = ?, started = ?, log = ?, with_shas = ?`, func(it worktree.AuditItem) []any {
		var with []string
		for _, o := range items {
			if o.SHA != it.SHA {
				with = append(with, o.SHA)
			}
		}
		return []any{dir, tag, start.Unix(), logPath, strings.Join(with, " ")}
	})
}

// finish ends each item's row with the run's result.
func (w *worker) finish(ctx context.Context, items []worktree.AuditItem, logOf func(worktree.AuditItem) string, code, secs int) error {
	err := w.update(ctx, items, `UPDATE audits SET state = 'done', code = ?, secs = ?, log = ?, ended = ?`,
		func(it worktree.AuditItem) []any { return []any{code, secs, logOf(it), time.Now().Unix()} })
	if code != 0 { // a lost publication still broadcasts; the failure id dedupes a rerun
		for _, it := range items {
			worktree.ReportAuditFailure(it, code, logOf(it))
		}
	}
	return err
}

// requeue puts items back in the queue, never failed: every slot is busy, so a later worker runs them.
func (w *worker) requeue(ctx context.Context, items []worktree.AuditItem) error {
	return errors.Join(worktree.ErrSlotBusy, w.update(ctx, items, `UPDATE audits SET state = 'queued', slot = '', run = ''`,
		func(worktree.AuditItem) []any { return nil }))
}

// fail ends items that could not run: the error in each one's own log, exit 1, so none hangs silently.
func (w *worker) fail(ctx context.Context, items []worktree.AuditItem, cause error) error {
	logs := map[worktree.AuditItem]string{}
	var errs []error
	for i, it := range items {
		logs[it] = filepath.Join(worktree.AuditDir(), fmt.Sprintf("fail-%d-%d-%s.log", time.Now().UnixNano(), i, worktree.ShortOID(it.SHA)))
		errs = append(errs, os.WriteFile(logs[it], []byte("audit: not run: "+cause.Error()+"\n"), 0o600))
	}
	return errors.Join(append(errs, w.finish(ctx, items, func(it worktree.AuditItem) string { return logs[it] }, 1, 0))...)
}

// prune imports legacy records and drops rows done before AuditKeep, then unreferenced files past it.
func (w *worker) prune(ctx context.Context) error {
	cut := time.Now().Add(-worktree.AuditKeep)
	var legacy []string
	kept := map[string]bool{}
	if err := w.publish(ctx, func(tx *sql.Tx, now int64) error {
		var err error
		if legacy, err = importLegacy(ctx, tx, now); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM audits WHERE state = 'done' AND ended < ?`, cut.Unix()); err != nil {
			return err
		}
		clear(kept)
		rows, err := allRows(ctx, tx)
		for _, r := range rows {
			kept[r.Log] = true
		}
		return err
	}); err != nil {
		return err
	}
	for _, base := range legacy {
		for _, ext := range []string{".json", ".exit", ".reported", ".hung"} {
			_ = os.Remove(base + ext)
		}
	}
	removeOld(cut, kept)
	trees, _ := filepath.Glob(filepath.Join(worktree.SlotRoot(), "tree-audit-*"))
	for _, t := range trees { // a fallback tree an unswept run kept, once nothing is in it
		worktree.RemoveIdleTree(ctx, t)
	}
	return nil
}

// removeOld deletes markers and logs last written before cut, except logs a kept row still names.
func removeOld(cut time.Time, kept map[string]bool) {
	for _, pat := range []string{"*.log", "*.reported", "*.hung", "*.exit"} {
		files, _ := filepath.Glob(filepath.Join(worktree.AuditDir(), pat))
		for _, f := range files {
			if fi, err := os.Stat(f); err == nil && fi.ModTime().Before(cut) && !kept[f] {
				_ = os.Remove(f)
			}
		}
	}
}

// legacyRecord names a file-based record, <sha8>.json; verdicts.json never matches.
var legacyRecord = regexp.MustCompile(`^[0-9a-f]{8}\.json$`)

// importLegacy turns file-based records into rows: finished ones done, unfinished ones queued to re-run alone.
func importLegacy(ctx context.Context, tx *sql.Tx, now int64) (bases []string, err error) {
	ents, _ := os.ReadDir(worktree.AuditDir())
	for _, e := range ents {
		if !legacyRecord.MatchString(e.Name()) {
			continue
		}
		base := filepath.Join(worktree.AuditDir(), strings.TrimSuffix(e.Name(), ".json"))
		r, ok := legacyRow(base, now)
		if !ok {
			continue
		}
		bases = append(bases, base)
		if r.State == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO audits(sha, queued, repo, base, state, started, ended,
			log, code, secs) VALUES(?, ?, ?, '', ?, ?, ?, ?, ?, ?)`, r.SHA, r.Queued, r.Repo, r.State, r.Started, r.Ended,
			r.Log, r.Code, r.Secs); err != nil {
			return nil, err
		}
	}
	return bases, nil
}

// legacyRow reads one record (no State: reported or too old); unfinished ones queue with an empty base, so run alone.
func legacyRow(base string, now int64) (r worktree.AuditRow, ok bool) {
	var rec struct {
		SHA, Repo, Log string
		Started        int64
	}
	if data, err := os.ReadFile(base + ".json"); err != nil || json.Unmarshal(data, &rec) != nil || rec.SHA == "" || rec.Repo == "" {
		return r, false
	}
	if _, err := os.Stat(base + ".reported"); err == nil || time.Since(time.Unix(rec.Started, 0)) > worktree.AuditKeep {
		return r, true
	}
	// the token comes from the record, so a replayed import is ignored; started=now keeps it from reading hung
	r = worktree.AuditRow{Repo: rec.Repo, SHA: rec.SHA, Queued: max(rec.Started, 1) * int64(time.Second),
		State: worktree.AuditQueued, Started: now}
	if !legacyExit(base, &r) && time.Since(time.Unix(rec.Started, 0)) < worktree.AuditStale {
		return r, false // its old audit may still be running: a later worker imports its result
	}
	if r.State == worktree.AuditDone {
		r.Started, r.Ended, r.Log = rec.Started, now, rec.Log
	}
	return r, true
}

// legacyExit reads base's exit file "<code> <seconds>" into r and marks it done; false while there is none.
func legacyExit(base string, r *worktree.AuditRow) bool {
	var code, secs int
	exit, err := os.ReadFile(base + ".exit")
	if err == nil {
		_, err = fmt.Sscanf(string(exit), "%d %d", &code, &secs) // a torn file leaves r untouched
	}
	if err == nil {
		r.State, r.Code, r.Secs = worktree.AuditDone, code, secs
	}
	return err == nil
}

// runJobs audits the claimed rows grouped by repo, in queue order.
func (w *worker) runJobs(ctx context.Context, jobs []worktree.AuditItem) error {
	var repos []string
	groups := map[string][]worktree.AuditItem{}
	for _, it := range jobs {
		if _, ok := groups[it.Repo]; !ok {
			repos = append(repos, it.Repo)
		}
		groups[it.Repo] = append(groups[it.Repo], it)
	}
	var errs []error
	for _, repo := range repos {
		errs = append(errs, w.auditRepo(ctx, repo, groups[repo]))
	}
	return errors.Join(errs...)
}

// auditRepo applies each item's diff onto HEAD in one slot and audits them together; misfits run alone after.
func (w *worker) auditRepo(parent context.Context, repo string, items []worktree.AuditItem) error {
	ctx, cancel := context.WithTimeout(parent, auditTimeout)
	defer cancel()
	own := "audit-" + strconv.Itoa(os.Getpid())
	head, err := proc.RunGit(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return w.fail(parent, items, err)
	}
	slot, err := worktree.AcquireSlot(ctx, repo, strings.TrimSpace(head), own)
	if errors.Is(err, worktree.ErrSlotBusy) {
		return w.requeue(parent, items)
	}
	if err != nil {
		return w.fail(parent, items, err)
	}
	var included, alone []worktree.AuditItem
	applied := false
	for _, it := range items {
		ok, staged := applyAudit(ctx, slot.Dir, it)
		if !ok {
			alone = append(alone, it)
			continue
		}
		included, applied = append(included, it), applied || staged
	}
	swept := true
	if err = commitAuditTree(ctx, slot.Dir, applied); err != nil {
		err = w.fail(parent, included, err)
	} else if len(included) > 0 {
		swept, err = w.runIn(parent, slot.Dir, repo, included)
	}
	if swept {
		slot.Release()
	}
	errs := []error{err}
	for _, it := range alone {
		errs = append(errs, w.auditAlone(parent, repo, it, own))
	}
	return errors.Join(errs...)
}

// auditAlone audits it on its own checkout, for a diff that did not apply onto HEAD.
func (w *worker) auditAlone(parent context.Context, repo string, it worktree.AuditItem, own string) error {
	ctx, cancel := context.WithTimeout(parent, auditTimeout) // fresh: the batch's deadline may be spent
	defer cancel()
	slot, err := worktree.AcquireSlot(ctx, repo, it.SHA, own)
	if errors.Is(err, worktree.ErrSlotBusy) {
		return w.requeue(parent, []worktree.AuditItem{it})
	}
	if err != nil {
		return w.fail(parent, []worktree.AuditItem{it}, err)
	}
	swept, err := w.runIn(parent, slot.Dir, repo, []worktree.AuditItem{it})
	if swept {
		slot.Release()
	}
	return err
}

// applyAudit reports whether it's change is in dir's tree (ok) and whether it staged it now (staged).
func applyAudit(ctx context.Context, dir string, it worktree.AuditItem) (ok, staged bool) {
	diff, err := proc.RunGitRaw(ctx, it.Repo, "diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-color",
		"--src-prefix=a/", "--dst-prefix=b/", it.Base, it.SHA) // canonical patch: user diff settings must not drop changes
	if err != nil {
		return false, false
	}
	if strings.TrimSpace(diff) == "" {
		return true, false
	}
	f, err := os.CreateTemp(worktree.AuditDir(), "patch-*.diff")
	if err != nil {
		return false, false
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err = f.WriteString(diff); errors.Join(err, f.Close()) != nil {
		return false, false
	}
	if _, err := proc.RunGit(ctx, dir, "apply", "--reverse", "--check", f.Name()); err == nil {
		return true, false
	}
	_, err = proc.RunGit(ctx, dir, "apply", "--index", f.Name())
	return err == nil, err == nil
}

// commitAuditTree commits the staged diffs in the slot so `make audit`'s clean-tree check passes; hooks never run.
func commitAuditTree(ctx context.Context, dir string, applied bool) error {
	if !applied {
		return nil
	}
	_, err := proc.RunGit(ctx, dir, "-c", "user.name=claude-code", "-c", "user.email=audit@localhost",
		"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null",
		"commit", "-q", "--no-verify", "--allow-empty", "-m", "audit: landed diffs") // diffs may cancel out
	return err
}

// runIn runs the audit once in dir for items, sharing one log, and ends each item's row; swept: nothing is left in dir.
func (w *worker) runIn(parent context.Context, dir, repo string, items []worktree.AuditItem) (swept bool, err error) {
	start := time.Now()
	logPath := filepath.Join(worktree.AuditDir(), fmt.Sprintf("run-%d-%s.log", start.UnixNano(), worktree.ShortOID(items[0].SHA)))
	logFile, err := os.Create(logPath)
	if err != nil {
		return true, w.fail(parent, items, err)
	}
	defer logFile.Close()
	ctx, cancel := context.WithTimeout(parent, auditTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", worktree.AuditCommand())
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, logFile, logFile
	cmd.WaitDelay = auditGrace + proc.ExecWaitDelay // the tree sweep finishes before Run returns
	// -trimpath, one inherited GOCACHE: a fresh tree per run still hits the test cache (fast-land 6.1)
	cmd.Env = append(os.Environ(), proc.GoFlagsEntry(os.Environ(), "-trimpath"))
	proc.TreeCancel(cmd, auditGrace) // a timeout kills every descendant, make's own `set -m` groups included
	if err := w.markRun(parent, items, dir, proc.TreeTag(cmd), logPath, start); err != nil {
		return true, err
	}
	linkVenvs(repo, dir)
	code := 0
	if err := cmd.Run(); err != nil {
		code = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() > 0 {
			code = ee.ExitCode()
		}
	}
	sweep := proc.TreeSweep(cmd) // every exit: a background child the audit left must not outlive the slot
	if sweep != nil {
		_, _ = fmt.Fprintln(logFile, "audit:", sweep)
	}
	return sweep == nil, errors.Join(sweep, w.finish(parent, items, func(worktree.AuditItem) string { return logPath }, code, int(time.Since(start).Seconds())))
}

// linkVenvs symlinks the repo's untracked .venv/venv into dir so Python audits find their interpreter.
func linkVenvs(repo, dir string) {
	for _, d := range []string{".venv", "venv"} {
		src, dst := filepath.Join(repo, d), filepath.Join(dir, d)
		if fi, err := os.Stat(src); err == nil && fi.IsDir() {
			if _, err := os.Lstat(dst); os.IsNotExist(err) {
				_ = os.Symlink(src, dst)
			}
		}
	}
}
