package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

const streamBuffer = 1024

var streamStall = 2 * time.Minute

// ErrStreamAbandoned is the cancellation cause when a stream stays full beyond streamStall.
var ErrStreamAbandoned = errors.New("agent: stream consumer abandoned")

type chanSink struct {
	mu        sync.Mutex
	ch        chan Event
	cancel    context.CancelCauseFunc
	start     time.Time
	seq       int64
	host      string
	fullSince time.Time
	dropped   int
	sawResult bool
}

func newChanSink(cancel context.CancelCauseFunc) *chanSink {
	host, _ := os.Hostname()
	return &chanSink{ch: make(chan Event, streamBuffer), cancel: cancel, start: clock(), host: host}
}

// Emit stamps ev and drops the oldest buffered event when the consumer falls behind.
func (s *chanSink) Emit(ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	ev.Schema, ev.Seq, ev.TMs = EventSchema, s.seq, clock().Sub(s.start).Milliseconds()
	if ev.Host == "" {
		ev.Host = s.host
	}
	if ev.Type == EventResult {
		s.sawResult = true
	}
	select {
	case s.ch <- ev:
		s.fullSince = time.Time{}
		return nil
	default:
	}
	if s.fullSince.IsZero() {
		s.fullSince = clock()
	} else if clock().Sub(s.fullSince) > streamStall {
		s.cancel(ErrStreamAbandoned)
	}
	select {
	case <-s.ch:
		s.dropped++
	default:
	}
	select {
	case s.ch <- ev:
	default:
	}
	return nil
}

// Stream runs a dispatch and returns its buffered event stream.
func (c *Client) Stream(ctx context.Context, o RunOptions) <-chan Event {
	return c.streamWith(ctx, cmp.Or(o.Agent, "claude"), func(ctx context.Context, sink Sink) (Result, error) {
		return c.Run(ctx, o, sink)
	})
}

func (c *Client) stream(ctx context.Context, p *Prepared) <-chan Event {
	return c.streamWith(ctx, cmp.Or(p.Opts.Agent, "claude"), func(ctx context.Context, sink Sink) (Result, error) {
		return c.Exec(ctx, p, sink, DispatchOpts{})
	})
}

func (c *Client) streamWith(
	ctx context.Context, agentName string, fn func(context.Context, Sink) (Result, error),
) <-chan Event {
	ctx, cancel := context.WithCancelCause(ctx)
	sink := newChanSink(cancel)
	go func() {
		defer close(sink.ch)
		defer cancel(nil)
		var err error
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
			sink.mu.Lock()
			sawResult := sink.sawResult
			sink.mu.Unlock()
			if err != nil && !sawResult {
				_ = sink.Emit(ResultEvent(FailureEnvelope(agentName, err)))
			}
			sink.mu.Lock()
			dropped := sink.dropped
			sink.mu.Unlock()
			if dropped > 0 && c.Notify != nil {
				c.Notify(fmt.Sprintf("agent: stream dropped %d events", dropped))
			}
		}()
		_, err = fn(ctx, sink)
	}()
	return sink.ch
}
