package tui

import (
	"fmt"
	"os"
	"sync"
)

// Terminal is the raw-mode session: cursor hidden, keys decoded onto a channel. Close restores
// everything; callers defer it so a panic can't strand the shell in raw mode.
type Terminal struct {
	fd   int
	prev *termState
	out  *os.File
	in   *os.File
	keys chan Key
	stop chan struct{} // closed by Close — ends the key pump
	done chan struct{} // closed by the key pump when it exits
	once sync.Once
}

// Open puts stdin into raw mode and hides the cursor. Fails cleanly when stdin is not a TTY, so
// callers fall back to a line-mode prompt instead of crashing.
func Open() (*Terminal, error) {
	fd := int(os.Stdin.Fd())
	if !isTerminal(fd) {
		return nil, fmt.Errorf("stdin is not a terminal")
	}
	prev, err := makeRaw(fd)
	if err != nil {
		return nil, err
	}
	t := &Terminal{fd: fd, prev: prev, out: os.Stdout, in: os.Stdin, keys: make(chan Key, 8),
		stop: make(chan struct{}), done: make(chan struct{})}
	fmt.Fprint(t.out, "\x1b[?25l") // hide cursor
	go t.readKeys()
	return t, nil
}

// Close restores the terminal: cooked mode, cursor back, key pump stopped. Safe to call more than once.
func (t *Terminal) Close() {
	t.stopReader()
	fmt.Fprint(t.out, "\x1b[?25h")
	_ = restore(t.fd, t.prev)
}

// stopReader ends the key pump and waits for it: an orphaned read(2) on stdin would otherwise win the
// next line the user types after Close returns. Unpollable stdin keeps the old parked-forever reader.
func (t *Terminal) stopReader() {
	if t.stop == nil {
		return
	}
	t.once.Do(func() { close(t.stop) })
	if canPoll {
		<-t.done
	}
}

// Keys is the decoded keypress stream.
func (t *Terminal) Keys() <-chan Key { return t.keys }

// Size returns the terminal's current width and height (80x24 when the query fails).
func (t *Terminal) Size() (w, h int) {
	w, h, err := getSize(t.fd)
	if err != nil || w <= 0 || h <= 0 {
		return 80, 24
	}
	return w, h
}

// readKeys pumps stdin through drain. A partial sequence at the buffer end carries into the next
// read, so an escape sequence split across reads still decodes. Exits when stopReader signals.
func (t *Terminal) readKeys() {
	defer close(t.done)
	var pending []byte
	buf := make([]byte, 64)
	for {
		select {
		case <-t.stop:
			return
		default:
		}
		if !waitReadable(t.fd, pollTick) {
			continue // nothing typed yet — re-check stop instead of parking in read(2)
		}
		n, err := t.in.Read(buf)
		if n > 0 {
			var keys []Key
			keys, pending = drain(append(pending, buf[:n]...))
			for _, key := range keys {
				select {
				case t.keys <- key:
				case <-t.stop:
					return
				}
			}
		}
		if err != nil {
			close(t.keys)
			return
		}
	}
}
