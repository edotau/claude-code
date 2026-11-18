package redact

import "io"

// Writer masks vault values in what is written to w, even one split across writes; Close flushes the held tail.
func Writer(w io.Writer) io.WriteCloser { return &streamWriter{w: w} }

type streamWriter struct {
	w    io.Writer
	hold []byte // the last longest-1 bytes, which may start a value finished by the next write
	buf  []byte
}

func (s *streamWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	m := Shared()
	if m.longest == 0 {
		if _, err := s.w.Write(p); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	s.buf = append(append(s.buf[:0], s.hold...), p...) // a copy: an io.Writer must not modify p
	m.MaskValues(s.buf)
	keep := min(len(s.buf), m.longest-1)
	cut := len(s.buf) - keep
	s.hold = append(s.hold[:0], s.buf[cut:]...)
	if cut == 0 {
		return len(p), nil
	}
	if _, err := s.w.Write(s.buf[:cut]); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *streamWriter) Close() error {
	if len(s.hold) == 0 {
		return nil
	}
	_, err := s.w.Write(s.hold)
	s.hold = nil
	return err
}
