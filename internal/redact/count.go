package redact

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"sync"
)

const CountWindow = 1 << 20

// countPiece is the least text masked at once: a shape regex then scans only pieces holding its trigger word.
const countPiece = 8 << 10

// Counter counts what Mask would mask in a file, running Mask over pieces cut at line breaks no match crosses.
type Counter struct {
	matcher       *Matcher
	window, piece int
	buffers       sync.Pool
}

type countBuffers struct{ data, scratch []byte }

func NewCounter(m *Matcher) *Counter {
	return &Counter{matcher: m, window: CountWindow, piece: countPiece}
}

func (c *Counter) Matcher() *Matcher { return c.matcher }

func (c *Counter) File(path string) (hits int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, errors.New("redact: count open failed")
	}
	defer func() {
		if f.Close() != nil {
			err = errors.New("redact: count close failed")
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return 0, errors.New("redact: count stat failed")
	}
	bufs, _ := c.buffers.Get().(*countBuffers)
	if bufs == nil {
		bufs = &countBuffers{}
	}
	hits, err = c.count(f, bufs)
	if cap(bufs.data) <= 4*c.window { // a buffer grown for one huge unbroken stretch is not kept
		c.buffers.Put(bufs)
	}
	if err != nil {
		return 0, err
	}
	current, err := f.Stat()
	if err != nil || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
		return 0, errors.New("redact: file changed during count")
	}
	return hits, nil
}

// count reads r a window at a time and counts every cut piece; the rest is masked whole at end of input.
func (c *Counter) count(r io.Reader, bufs *countBuffers) (int, error) {
	hits, from := 0, 0
	buf := bufs.data[:0]
	defer func() { bufs.data = buf[:0] }()
	for {
		var eof bool
		var err error
		buf, eof, err = readUpTo(r, buf, len(buf)+c.window)
		if err != nil {
			return 0, err
		}
		n, cut, stop := c.segment(buf, from, bufs)
		hits += n
		if eof {
			return hits + c.matcher.Mask(buf[cut:]), nil
		}
		buf = buf[:copy(buf, buf[cut:])]
		from = stop - cut
	}
}

func readUpTo(r io.Reader, buf []byte, need int) ([]byte, bool, error) {
	buf = slices.Grow(buf, need-len(buf))
	for len(buf) < need {
		n, err := r.Read(buf[len(buf):need])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			return buf, true, nil
		}
		if err != nil {
			return buf, false, errors.New("redact: count read failed")
		}
	}
	return buf, false, nil
}

// segment counts b in pieces ending at line breaks no match crosses, searching from from; stop is where it ended.
func (c *Counter) segment(b []byte, from int, bufs *countBuffers) (hits, cut, stop int) {
	end := max(from, c.piece)
	for end < len(b) {
		i := bytes.IndexByte(b[end:], '\n')
		if i < 0 {
			return hits, cut, len(b)
		}
		end += i + 1
		if c.matcher.crosses(b, end) {
			continue
		}
		bufs.scratch = append(bufs.scratch[:0], b[cut:end]...)
		n, open := c.matcher.mask(bufs.scratch)
		if open {
			end = cut + 2*(end-cut) // doubling keeps a long open PEM block from being masked once per line
			continue
		}
		hits, cut, end = hits+n, end, end+c.piece
	}
	return hits, cut, end
}

// crosses reports a value occurrence spanning offset p of b, or one the bytes after p cannot yet settle.
func (m *Matcher) crosses(b []byte, p int) bool {
	if !m.breaks && len(b)-p >= m.longest { // b[p-1] is a line break, which no value then holds
		return false
	}
	for i := max(0, p-m.longest+1); i < p; i++ {
		if n, ok := m.matchAt(b[i:], false); !ok || i+n > p {
			return true
		}
	}
	return false
}
