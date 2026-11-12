package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/agent/wire"
)

// tailBeat is the silence before a follow prints its heartbeat; tailPoll is the log re-read interval. Tests swap both.
var (
	tailBeat = 15 * time.Second
	tailPoll = 250 * time.Millisecond
)

// Tail copies ref's log (default: name's newest job in cwd's repo) to w; follow heartbeats until the record stops.
func Tail(ctx context.Context, name, cwd, ref string, follow bool, w io.Writer) (Record, error) {
	rec, err := resolveJob(ctx, name, cwd, ref, nil)
	if err != nil {
		return Record{}, err
	}
	rec, f, err := openTail(ctx, rec, follow, Record.LogPath)
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
	return followLog(ctx, rec, f, w)
}

func followLog(ctx context.Context, rec Record, f *os.File, w io.Writer) (Record, error) {
	lastOut, lastBeat := time.Now(), time.Time{}
	for rec.Alive() {
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
			if _, err := fmt.Fprintf(w, "· no output for %s (elapsed %s)\n", quiet.Round(time.Second), now.Sub(rec.Time).Round(time.Second)); err != nil {
				return rec, err
			}
		}
	}
	return rec, nil // the last pass reloaded before it copied, so nothing landed after it
}

// TailEvents copies rec's <id>.events.jsonl to w; follow waits until the record leaves running, heartbeating.
func TailEvents(ctx context.Context, rec Record, follow bool, w io.Writer) (Record, error) {
	rec, f, err := openTail(ctx, rec, follow, Record.EventsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rec, fmt.Errorf("job %s has no event stream (it predates event logs, or never started)", rec.ID)
		}
		return rec, err
	}
	defer f.Close()
	return followEvents(ctx, rec, follow, w, bufio.NewReader(f))
}

func openTail(ctx context.Context, rec Record, follow bool, path func(Record) string) (Record, *os.File, error) {
	f, err := os.Open(path(rec))
	for follow && errors.Is(err, os.ErrNotExist) && rec.Alive() {
		if err = sleepCtx(ctx, tailPoll); err != nil {
			return rec, nil, err
		}
		rec = reloaded(rec)
		f, err = os.Open(path(rec))
	}
	return rec, f, err
}

func followEvents(ctx context.Context, rec Record, follow bool, w io.Writer, br *bufio.Reader) (Record, error) {
	var pend []byte
	lastOut, lastBeat := time.Now(), time.Time{}
	for {
		n, err := copyEventLines(br, &pend, w)
		if err != nil || !follow {
			return reloaded(rec), err
		}
		if n > 0 {
			lastOut = time.Now()
		}
		if !rec.Alive() { // the writer is gone, and rec was reloaded before this copy
			return rec, nil
		}
		if err := sleepCtx(ctx, tailPoll); err != nil {
			return rec, err
		}
		rec = reloaded(rec)
		if lastBeat, err = eventHeartbeat(w, rec, n, lastOut, lastBeat); err != nil {
			return rec, err
		}
	}
}

// eventHeartbeat writes a heartbeat event when due; lastBeat advances only once the write lands.
func eventHeartbeat(w io.Writer, rec Record, n int, lastOut, lastBeat time.Time) (time.Time, error) {
	if now := time.Now(); n == 0 && now.Sub(lastBeat) >= tailBeat && now.Sub(lastOut) >= tailBeat {
		_, err := fmt.Fprintf(w, "{\"schema\":%d,\"dispatch_id\":%q,\"type\":\"heartbeat\",\"t_ms\":%d}\n",
			wire.EventSchema, rec.ID, now.Sub(rec.Time).Milliseconds())
		if err != nil {
			return lastBeat, err
		}
		return now, nil
	}
	return lastBeat, nil
}

// copyEventLines writes each complete line br has to w, holding a partial tail in pend.
func copyEventLines(br *bufio.Reader, pend *[]byte, w io.Writer) (int, error) {
	n := 0
	for {
		chunk, rerr := br.ReadBytes('\n')
		*pend = append(*pend, chunk...)
		if rerr != nil { // EOF: the rest of the line has not landed yet
			if errors.Is(rerr, io.EOF) {
				return n, nil
			}
			return n, rerr
		}
		if _, err := w.Write(*pend); err != nil {
			return n, err
		}
		n += len(*pend)
		*pend = (*pend)[:0]
	}
}

// reloaded re-reads rec from the dispatch log; a record pruned mid-follow keeps its last known state.
func reloaded(rec Record) Record {
	if latest, err := LoadRecord(rec.ID); err == nil {
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
