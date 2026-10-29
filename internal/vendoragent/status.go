package vendoragent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/secrets/shapes"
)

// known reports whether name is an agent with or without a vendor Adapter (claude has none).
func known(name string) bool {
	if _, ok := Lookup(name); ok {
		return true
	}
	_, err := agent.Lookup(name)
	return err == nil
}

// Find resolves ref (exact id or unique prefix) across every agent's records, so the caller learns the agent.
func Find(ref string) (agent.Record, error) {
	return Resolve(agent.RecentRecords(1<<20), ref, func(agent.Record) bool { return false })
}

// Row is one background-job line for `claude-code <v> status`.
type Row struct {
	ID       string
	Status   string // running | lost | ok | error (lost = running record, dead pid)
	Elapsed  string
	Preview  string
	Session  string
	Error    string `json:",omitempty"` // the failed dispatch's redacted error text
	Progress string `json:",omitempty"` // "part 2/3" while a split review runs
}

// Status lists rows for name's jobs in cwd's repo (all repos when all is set), newest first; ref narrows via Resolve.
func Status(ctx context.Context, name, cwd, ref string, all bool) ([]Row, error) {
	if !known(name) {
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
	if !known(name) {
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
	if !known(name) {
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

// TailEvents copies rec's <id>.events.jsonl to w; with follow it waits for a result event or until the record
// leaves running, printing a heartbeat event per tailBeat of silence. It returns the record as last seen.
func TailEvents(ctx context.Context, rec agent.Record, follow bool, w io.Writer) (agent.Record, error) {
	f, err := os.Open(rec.EventsPath())
	for follow && errors.Is(err, os.ErrNotExist) && Alive(rec) { // the job may not have started yet
		if err = sleepCtx(ctx, tailPoll); err != nil {
			return rec, err
		}
		rec = reloaded(rec)
		f, err = os.Open(rec.EventsPath())
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rec, fmt.Errorf("job %s has no event stream (it predates event logs, or never started)", rec.ID)
		}
		return rec, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	var pend []byte
	lastOut, lastBeat := time.Now(), time.Time{}
	for {
		n, done, err := copyEventLines(br, &pend, w)
		if err != nil || done || !follow {
			return reloaded(rec), err
		}
		if n > 0 {
			lastOut = time.Now()
		}
		if !Alive(rec) { // the writer is gone: one last drain, then stop
			_, _, err = copyEventLines(br, &pend, w)
			return rec, err
		}
		if err := sleepCtx(ctx, tailPoll); err != nil {
			return rec, err
		}
		rec = reloaded(rec)
		if now := time.Now(); n == 0 && now.Sub(lastBeat) >= tailBeat && now.Sub(lastOut) >= tailBeat {
			lastBeat = now
			fmt.Fprintf(w, "{\"schema\":%d,\"dispatch_id\":%q,\"type\":\"heartbeat\",\"t_ms\":%d}\n",
				agent.EventSchema, rec.ID, now.Sub(rec.Time).Milliseconds())
		}
	}
}

// copyEventLines writes each complete line br has to w, holding a partial tail in pend; done = a result line passed.
func copyEventLines(br *bufio.Reader, pend *[]byte, w io.Writer) (n int, done bool, err error) {
	for {
		chunk, rerr := br.ReadBytes('\n')
		*pend = append(*pend, chunk...)
		if rerr != nil { // EOF: the rest of the line has not landed yet
			return n, done, nil
		}
		if _, err := w.Write(*pend); err != nil {
			return n, done, err
		}
		var ev struct{ Type string }
		if json.Unmarshal(*pend, &ev) == nil && ev.Type == agent.EventResult {
			done = true
		}
		n += len(*pend)
		*pend = (*pend)[:0]
		if done {
			return n, true, nil
		}
	}
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
	if !known(name) {
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
	elapsed := r.Elapsed()
	if status == "running" || status == "lost" {
		elapsed = time.Since(r.Time).Round(time.Second).String()
	}
	preview := agent.Preview(r.Request.Prompt, 60)
	return Row{ID: r.ID, Status: status, Elapsed: elapsed, Preview: preview, Session: r.SessionID,
		Error: shapes.Redact(r.Error), Progress: r.Progress}
}
