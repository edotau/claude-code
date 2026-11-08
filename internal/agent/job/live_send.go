package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
)

// SendLive sends one turn to live session id and streams that turn's events to sink until its result.
func SendLive(ctx context.Context, id, prompt string, sink wire.Sink) (wire.Envelope, error) {
	rec, err := LoadRecord(id)
	if err != nil {
		return wire.Envelope{}, err
	}
	if !rec.Live {
		return wire.Envelope{}, fmt.Errorf("dispatch %s is not a live session (start one: ask --agent claude|gemini --live)", rec.ID)
	}
	if !rec.Alive() {
		return wire.Envelope{}, fmt.Errorf("live session %s has ended (%s); start a new one", rec.ID, rec.Status)
	}
	conn, err := (&net.Dialer{Timeout: liveIOTimeout}).DialContext(ctx, "unix", LiveSocket(rec.ID))
	if err != nil {
		return wire.Envelope{}, fmt.Errorf("live session %s: %w (claude-code agents reap clears dead sessions)", rec.ID, err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetWriteDeadline(time.Now().Add(liveIOTimeout))
	if err := json.NewEncoder(conn).Encode(agent.LiveRequest{Schema: agent.LiveSchema, Prompt: prompt}); err != nil {
		return wire.Envelope{}, err
	}
	return receiveTurn(ctx, rec.ID, json.NewDecoder(conn), sink)
}

func receiveTurn(ctx context.Context, id string, dec *json.Decoder, sink wire.Sink) (wire.Envelope, error) {
	for {
		var ev wire.Event
		if err := dec.Decode(&ev); err != nil {
			if ctx.Err() != nil {
				return wire.Envelope{}, ctx.Err()
			}
			return wire.Envelope{}, fmt.Errorf("live session %s closed before the turn's result: %w", id, err)
		}
		if err := refusalError(ev); err != nil {
			return wire.Envelope{}, err
		}
		if err := sink.Emit(ev); err != nil {
			return wire.Envelope{}, err
		}
		if ev.Type == wire.EventResult {
			if ev.Envelope == nil {
				return wire.Envelope{}, errors.New("result event without an envelope")
			}
			return *ev.Envelope, nil
		}
	}
}

func refusalError(ev wire.Event) error {
	if ev.Type != wire.EventError {
		return nil
	}
	switch ev.Status {
	case statusBusy:
		return ErrLiveBusy
	case statusRefused:
		return errors.New(ev.Text)
	}
	return nil
}

// liveReadyWait bounds how long `ask --live` waits for the holder to open its socket.
const liveReadyWait = 20 * time.Second

// IdleOf is the idle limit a live session runs under: the request's timeout, else the default.
func IdleOf(req agent.RunOptions) time.Duration {
	if req.Timeout > 0 {
		return req.Timeout
	}
	return DefaultLiveIdle
}

// WaitLiveReady polls for the holder's socket; it gives up early when the holder's record leaves running.
func WaitLiveReady(id string, pid int) error {
	deadline := time.Now().Add(liveReadyWait)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(LiveSocket(id)); err == nil {
			return nil
		}
		if rec, err := LoadRecord(id); err == nil && rec.Status != "running" {
			return errors.New(rec.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("no socket after %s (holder pid %d)", liveReadyWait, pid)
}
