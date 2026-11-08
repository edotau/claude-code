package job

import (
	"io"
	"sync"
)

// pumpBuf is an unbounded in-memory pipe draining claude stdout, so a stdin write never blocks behind unread output.
type pumpBuf struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
	done   chan struct{} // closed once the reader hit EOF or an error: all of stdout is in buf
}

func newPumpBuf(src io.Reader) *pumpBuf {
	b := &pumpBuf{done: make(chan struct{})}
	b.cond = sync.NewCond(&b.mu)
	go func() {
		chunk := make([]byte, 32<<10)
		for {
			n, err := src.Read(chunk)
			b.mu.Lock()
			b.buf = append(b.buf, chunk[:n]...)
			b.closed = err != nil
			b.cond.Broadcast()
			b.mu.Unlock()
			if err != nil {
				close(b.done)
				return
			}
		}
	}()
	return b
}

// Read blocks until data or the end of claude's stdout.
func (b *pumpBuf) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.buf) == 0 && !b.closed {
		b.cond.Wait()
	}
	if len(b.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.buf)
	b.buf = b.buf[n:]
	return n, nil
}
