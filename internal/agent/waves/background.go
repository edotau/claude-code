package waves

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
	"github.com/edotau/claude-code/internal/worktree"
)

// Background detaches the wave runner using the shared dispatch records and spawn machinery.
func Background(ctx context.Context, o Options, args []string) int {
	rec := agent.ClaimRecord("waves", agent.RunOptions{WorkDir: o.Dir, Prompt: o.Plan, Background: true}, time.Now())
	rec.Repo, rec.Log = agent.RepoRoot(ctx, o.Dir), rec.LogPath()
	if err := rec.Save(); err != nil {
		fmt.Fprintln(o.Err, err)
		return 1
	}
	if err := agent.SpawnArgvContext(ctx, &rec, backgroundArgs(args, rec.ID), nil); err != nil {
		rec.Finish(wire.Result{}, err, 0)
		if saveErr := rec.Save(); saveErr != nil {
			err = errors.Join(err, saveErr)
		}
		fmt.Fprintln(o.Err, err)
		return 1
	}
	if err := agent.WriteJSONLine(o.Out, struct {
		ID  string `json:"id"`
		Log string `json:"log"`
	}{rec.ID, rec.Log}); err != nil {
		fmt.Fprintln(o.Err, err)
		return 1
	}
	return 0
}

func backgroundArgs(args []string, id string) []string {
	out := append([]string{"agents", "ask"}, args...)
	return append(out, "--background=false", "--job", id)
}

// RunJob claims its own pid before execution, then settles the shared record without a parent save race.
func RunJob(ctx context.Context, o Options, id string) int {
	return runJob(ctx, o, id, Run)
}

func runJob(ctx context.Context, o Options, id string, run func(context.Context, Options) int) int {
	rec, err := agent.LoadRecord(id)
	if err != nil {
		fmt.Fprintln(o.Err, err)
		return 1
	}
	if rec.Agent != "waves" || rec.Status != "running" || rec.Request.Prompt != o.Plan {
		fmt.Fprintln(o.Err, "not a running wave job for this plan")
		return 2
	}
	if rec, err = agent.ClaimJob(rec.ID); err != nil {
		fmt.Fprintln(o.Err, err)
		if errors.Is(err, agent.ErrJobNotFresh) {
			return 2
		}
		return 1
	}
	rec.Log = rec.LogPath()
	notices := &waveNotices{session: noticeSession(rec)}
	o.notify = notices.wave
	start := time.Now()
	code := run(ctx, o)
	if ctx.Err() != nil {
		code = 4
	}
	code = finishJob(&rec, o, code, time.Since(start))
	notices.done(planSlug(o.Plan), rec.ID, code)
	return code
}

// noticeSession is the session that launched the job: its tool env, else the record owner's registration.
func noticeSession(rec agent.Record) string {
	if s := os.Getenv(paths.SessionIDEnv); s != "" {
		return s
	}
	return worktree.OwnerSession(rec.Owner)
}

// noticeFails caps the failed leaves one wave notice names; notices cap at 5 per prompt, so one line per wave.
const noticeFails = 2

// waveNotices posts one rolled-up line per finished wave and one at the end to the launching session.
type waveNotices struct {
	session   string
	ok, total int
}

func (n *waveNotices) wave(w WaveState, report string) {
	ok := 0
	var fails []string
	for _, l := range w.Leaves {
		if leafMark(l) == "ok" {
			ok++
		} else {
			fails = append(fails, l.ID+" ("+noticeReason(l, w)+")")
		}
	}
	n.ok, n.total = n.ok+ok, n.total+len(w.Leaves)
	line := fmt.Sprintf("wave %d (gates only): %d/%d ok", w.N, ok, len(w.Leaves))
	if len(fails) > 0 {
		line += fmt.Sprintf(", %d FAIL %s", len(fails), strings.Join(fails[:min(len(fails), noticeFails)], ", "))
		if len(fails) > noticeFails {
			line += fmt.Sprintf(" +%d more", len(fails)-noticeFails)
		}
	}
	if v := w.Verifier; v != nil && v.Exit != 0 {
		line += fmt.Sprintf(", wave gate: %s exit %d", noticeCmd(v.Cmd), v.Exit)
	}
	n.post(line + "; report " + report)
}

// noticeReason is a failed leaf's verify command and exit, else the wave report's reason.
func noticeReason(l LeafState, w WaveState) string {
	if v := l.Verify; v != nil && v.Exit != 0 {
		return fmt.Sprintf("verify: %s exit %d", noticeCmd(v.Cmd), v.Exit)
	}
	return leafReason(l, w)
}

func noticeCmd(cmd string) string {
	return textutil.CapRunes(strings.Join(strings.Fields(cmd), " "), 60, "…")
}

func (n *waveNotices) done(slug, id string, code int) {
	status := "done"
	if code == 4 {
		status = "stopped"
	} else if code != 0 {
		status = fmt.Sprintf("failed exit %d", code)
	}
	n.post(fmt.Sprintf("waves %s: %s, %d/%d ok; result: claude-code agents result %s", slug, status, n.ok, n.total, id))
}

func (n *waveNotices) post(line string) {
	if n.session != "" {
		_ = worktree.Append(n.session, worktree.Notice{TS: time.Now().Unix(), From: "waves", Kind: "waves", Text: line})
	}
}

func finishJob(rec *agent.Record, o Options, code int, took time.Duration) int {
	var err error
	if code == 4 {
		err = errors.New("interrupted")
	} else if code != 0 {
		err = fmt.Errorf("exit %d", code)
	}
	rec.Finish(wire.Result{}, err, took)
	if err != nil {
		fmt.Fprintln(o.Err, err)
	}
	if err := rec.Save(); err != nil {
		fmt.Fprintln(o.Err, err)
		return 1
	}
	return code
}
