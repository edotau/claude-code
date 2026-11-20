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
	tailPoll = 25 * time.Millisecond
)

// SwapTailPoll sets the log re-read interval and returns the func that restores it; a test seam for other packages.
func SwapTailPoll(d time.Duration) (restore func()) {
	orig := tailPoll
	tailPoll = d
	return func() { tailPoll = orig }
}

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
	cp := func() (int, error) { n, err := io.Copy(w, f); return int(n), err }
	return followTail(ctx, rec, true, cp, func(rec Record, n int, lastOut, lastBeat time.Time) (time.Time, error) {
		if now := time.Now(); n == 0 && now.Sub(lastBeat) >= tailBeat && now.Sub(lastOut) >= tailBeat {
			_, err := fmt.Fprintf(w, "· %s\n", quietLine(rec, now.Sub(lastOut), now))
			return now, err
		}
		return lastBeat, nil
	})
}

// quietLine is the follower's quiet note: the progress line, else the bare quiet and elapsed times.
func quietLine(rec Record, quiet time.Duration, now time.Time) string {
	if rec.Status == "running" {
		return ProgressOf(rec, now).Line(progressWidth)
	}
	return fmt.Sprintf("no output for %s (elapsed %s)", quiet.Round(time.Second), now.Sub(rec.Time).Round(time.Second))
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
	br, pend := bufio.NewReader(f), []byte(nil)
	return followTail(ctx, rec, follow, func() (int, error) { return copyEventLines(br, &pend, w) },
		func(rec Record, n int, lastOut, lastBeat time.Time) (time.Time, error) {
			return eventHeartbeat(w, rec, n, lastOut, lastBeat)
		})
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

// followTail polls cp until rec leaves running; beat runs after each pass and returns the new lastBeat.
func followTail(ctx context.Context, rec Record, follow bool, cp func() (int, error),
	beat func(Record, int, time.Time, time.Time) (time.Time, error)) (Record, error) {
	lastOut, lastBeat := time.Now(), time.Time{}
	for {
		n, err := cp()
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
		if lastBeat, err = beat(rec, n, lastOut, lastBeat); err != nil {
			return rec, err
		}
	}
}

// eventHeartbeat writes a heartbeat event when due; lastBeat advances only once the write lands.
func eventHeartbeat(w io.Writer, rec Record, n int, lastOut, lastBeat time.Time) (time.Time, error) {
	if now := time.Now(); n == 0 && now.Sub(lastBeat) >= tailBeat && now.Sub(lastOut) >= tailBeat {
		_, err := fmt.Fprintf(w, "{\"schema\":%d,\"dispatch_id\":%q,\"type\":\"heartbeat\",\"t_ms\":%d,\"live\":%q}\n",
			wire.EventSchema, rec.ID, now.Sub(rec.Time).Milliseconds(), ProgressOf(rec, now).Line(progressWidth))
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
