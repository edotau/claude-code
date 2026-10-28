package vendoragent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// Row is one background-job line for `claude-code <v> status`.
type Row struct {
	ID      string
	Status  string // running | lost | ok | error (lost = running record, dead pid)
	Elapsed string
	Preview string
	Session string
}

// Status lists rows for name's jobs in cwd's repo (all repos when all is set), newest first; ref narrows via Resolve.
func Status(ctx context.Context, name, cwd, ref string, all bool) ([]Row, error) {
	if _, ok := Lookup(name); !ok {
		return nil, errUnknownAgent(name)
	}
	recs := List(name, RepoRoot(ctx, cwd), all || ref != "")
	if ref != "" {
		rec, err := Resolve(recs, ref, nil)
		if err != nil {
			return nil, err
		}
		recs = []agent.Record{rec}
	}
	rows := make([]Row, len(recs))
	for i, r := range recs {
		rows[i] = toRow(r)
	}
	return rows, nil
}

// Result returns ref's record (default: newest finished) and its log text; a running match errors.
func Result(ctx context.Context, name, cwd, ref string) (agent.Record, string, error) {
	if _, ok := Lookup(name); !ok {
		return agent.Record{}, "", errUnknownAgent(name)
	}
	recs := List(name, RepoRoot(ctx, cwd), ref != "") // an explicit id resolves across repos; the default stays local
	rec, err := Resolve(recs, ref, func(r agent.Record) bool { return r.Status != "running" })
	if err != nil {
		return agent.Record{}, "", err
	}
	if rec.Status == "running" {
		return agent.Record{}, "", fmt.Errorf("job %s is still running", rec.ID)
	}
	log, _ := os.ReadFile(rec.Log) // best-effort: a missing log still returns the record
	return rec, string(log), nil
}

// tailBeat is the silence before a follow prints its heartbeat; tailPoll is the log re-read interval. Tests swap both.
var (
	tailBeat = 15 * time.Second
	tailPoll = 250 * time.Millisecond
)

// Tail copies ref's log (default: name's newest job in cwd's repo) to w. With follow it keeps copying until the
// record stops being alive, printing a heartbeat line per tailBeat of silence. It returns the record as last seen.
func Tail(ctx context.Context, name, cwd, ref string, follow bool, w io.Writer) (agent.Record, error) {
	if _, ok := Lookup(name); !ok {
		return agent.Record{}, errUnknownAgent(name)
	}
	recs := List(name, RepoRoot(ctx, cwd), ref != "") // an explicit id resolves across repos; the default stays local
	rec, err := Resolve(recs, ref, nil)
	if err != nil {
		return agent.Record{}, err
	}
	f, err := os.Open(rec.LogPath())
	for follow && errors.Is(err, os.ErrNotExist) && Alive(rec) { // the CLI child may not have started yet
		if err = sleepCtx(ctx, tailPoll); err != nil {
			return rec, err
		}
		rec = reloaded(rec)
		f, err = os.Open(rec.LogPath())
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rec, fmt.Errorf("job %s has no log (it never started the CLI, or predates dispatch logs)", rec.ID)
		}
		return rec, err
	}
	defer f.Close()
	if _, err := io.Copy(w, f); err != nil || !follow {
		return reloaded(rec), err
	}
	lastOut, lastBeat := time.Now(), time.Time{}
	for Alive(rec) {
		if err := sleepCtx(ctx, tailPoll); err != nil {
			return rec, err
		}
		rec = reloaded(rec)
		n, err := io.Copy(w, f)
		if err != nil {
			return rec, err
		}
		now := time.Now()
		if n > 0 {
			lastOut = now
		} else if quiet := now.Sub(lastOut); now.Sub(lastBeat) >= tailBeat && quiet >= tailBeat {
			lastBeat = now
			fmt.Fprintf(w, "· no output for %s (elapsed %s)\n", quiet.Round(time.Second), now.Sub(rec.Time).Round(time.Second))
		}
	}
	_, err = io.Copy(w, f) // whatever landed between the last read and the record leaving running
	return rec, err
}

// reloaded re-reads rec from the dispatch log; a record pruned mid-follow keeps its last known state.
func reloaded(rec agent.Record) agent.Record {
	if latest, err := agent.LoadRecord(rec.ID); err == nil {
		return latest
	}
	return rec
}

// sleepCtx waits d, returning ctx.Err() if ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// CancelJob interrupts ref's job, default: the single running job for name in cwd's repo.
func CancelJob(ctx context.Context, name, cwd, ref string) (agent.Record, error) {
	if _, ok := Lookup(name); !ok {
		return agent.Record{}, errUnknownAgent(name)
	}
	recs := List(name, RepoRoot(ctx, cwd), ref != "") // an explicit id resolves across repos; the default stays local
	rec, err := Resolve(recs, ref, func(r agent.Record) bool { return Alive(r) })
	if err != nil {
		return agent.Record{}, err
	}
	if !Alive(rec) {
		return agent.Record{}, fmt.Errorf("job %s is not running", rec.ID)
	}
	if err := Cancel(&rec); err != nil {
		return agent.Record{}, err
	}
	return rec, nil
}

// toRow derives a status Row from a dispatch record; redaction matches printDispatchLog (internal/cli/agent.go).
func toRow(r agent.Record) Row {
	status := r.Status
	if status == "running" && !Alive(r) {
		status = "lost"
	}
	elapsed := r.Duration
	if status == "running" || status == "lost" {
		elapsed = time.Since(r.Time).Round(time.Second).String()
	}
	preview := agent.Preview(r.Request.Prompt, 60)
	return Row{ID: r.ID, Status: status, Elapsed: elapsed, Preview: preview, Session: r.SessionID}
}
