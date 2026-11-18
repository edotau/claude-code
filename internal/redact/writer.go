package redact

import "io"

// Writer masks vault values in what is written to w, even one split across writes; Close flushes the held tail.
func Writer(w io.Writer) io.WriteCloser { return &streamWriter{w: w} }

// JSONWriter is Writer for JSON or JSON lines: a mask keeps every value's type, so each document still parses.
func JSONWriter(w io.Writer) io.WriteCloser { return &streamWriter{w: w, json: &jsonLexer{}} }

type streamWriter struct {
	w    io.Writer
	json *jsonLexer
	pend []byte // from the first byte whose masking a later write could still change
	err  error  // the first failed write to w, returned by every later Write and by Close
}

func (s *streamWriter) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	m := Shared()
	s.pend = append(s.pend, p...) // a copy: an io.Writer must not modify p
	done := len(s.pend)
	if m.longest > 0 {
		_, done = m.scan(s.pend, false, s.json)
	}
	if err := s.emit(done); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *streamWriter) emit(n int) error {
	if n == 0 {
		return nil
	}
	written, err := s.w.Write(s.pend[:n])
	if err == nil && written != n {
		err = io.ErrShortWrite
	}
	if err != nil {
		s.err = err
		return err
	}
	s.pend = append(s.pend[:0], s.pend[n:]...)
	return nil
}

// Close masks and writes the held tail; it returns the first write error to w, including one an earlier Write hit.
func (s *streamWriter) Close() error {
	if s.err != nil {
		return s.err
	}
	Shared().scan(s.pend, true, s.json)
	err := s.emit(len(s.pend))
	s.pend = nil
	return err
}

// jsonLexer tracks string state across bytes; esc is -1 just after a backslash, else the \u hex digits left.
type jsonLexer struct {
	inString bool
	esc      int
}

func (l *jsonLexer) advance(c byte) {
	switch {
	case !l.inString:
		l.inString = c == '"'
	case l.esc == -1:
		l.esc = 0
		if c == 'u' {
			l.esc = 4
		}
	case l.esc > 0:
		l.esc--
	case c == '\\':
		l.esc = -1
	case c == '"':
		l.inString = false
	}
}

func (l *jsonLexer) skip(b []byte) {
	for _, c := range b {
		l.advance(c)
	}
}

// mask hides the string characters and escapes wholly inside b and turns number digits to 1; quotes and structure stay.
func (l *jsonLexer) mask(b []byte) {
	run := 0 // start of the pending run of string bytes to hide
	for i := 0; i < len(b); {
		n := l.unit(b[i:])
		switch {
		case n > 0:
			l.skip(b[i : i+n])
			i += n
			continue
		case !l.inString && b[i] >= '0' && b[i] <= '9':
			b[i] = '1' // a 1 is valid at any digit position, so the number stays a number
		}
		fill(b[run:i])
		l.advance(b[i])
		i++
		run = i
	}
	fill(b[run:])
}

// unit is the width of the string character or whole escape at b[0] when it lies inside b, else 0.
func (l *jsonLexer) unit(b []byte) int {
	if !l.inString || l.esc != 0 || b[0] == '"' {
		return 0
	}
	if b[0] != '\\' {
		return 1
	}
	if len(b) < 2 {
		return 0
	}
	n := 2
	if b[1] == 'u' {
		n = 6
	}
	if n > len(b) {
		return 0
	}
	return n
}
