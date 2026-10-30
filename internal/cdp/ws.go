// Package cdp is a minimal Chrome DevTools Protocol client over a stdlib RFC 6455 websocket.
package cdp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
)

const (
	maxFrame   = 16 << 20
	wsGUID     = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	opCont     = 0x0
	opText     = 0x1
	opClose    = 0x8
	opPing     = 0x9
	opPong     = 0xA
	headerFlag = 0x80
)

// wsConn is a client websocket: masked frames out, unmasked frames in.
type wsConn struct {
	c   net.Conn
	br  *bufio.Reader
	wmu sync.Mutex
}

// wsDial performs the client handshake against a ws:// URL and verifies Sec-WebSocket-Accept.
func wsDial(ctx context.Context, raw string) (*wsConn, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "ws" {
		return nil, fmt.Errorf("cdp: unsupported websocket url %q", raw)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		_ = c.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyRaw)
	req := "GET " + u.RequestURI() + " HTTP/1.1\r\nHost: " + u.Host + "\r\nUpgrade: websocket\r\n" +
		"Connection: Upgrade\r\nSec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		_ = c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	_ = resp.Body.Close() // the upgrade body is unused; frames are read through br
	sum := sha1.Sum([]byte(key + wsGUID))
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		_ = c.Close()
		return nil, fmt.Errorf("cdp: websocket handshake refused (%s)", resp.Status)
	}
	return &wsConn{c: c, br: br}, nil
}

// writeFrame sends one masked, final frame.
func (w *wsConn) writeFrame(op byte, p []byte) error {
	hdr := []byte{headerFlag | op}
	switch n := len(p); {
	case n < 126:
		hdr = append(hdr, headerFlag|byte(n))
	case n < 1<<16:
		hdr = append(hdr, headerFlag|126, byte(n>>8), byte(n))
	default:
		hdr = binary.BigEndian.AppendUint64(append(hdr, headerFlag|127), uint64(n))
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	body := make([]byte, len(p))
	for i, b := range p {
		body[i] = b ^ mask[i%4]
	}
	w.wmu.Lock()
	defer w.wmu.Unlock()
	_, err := w.c.Write(append(append(hdr, mask...), body...))
	return err
}

// readFrame reads one frame header and payload; server frames may not be masked.
func (w *wsConn) readFrame() (fin bool, op byte, p []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(w.br, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0F
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(w.br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(w.br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if h[1]&headerFlag != 0 {
		return false, 0, nil, errors.New("cdp: masked server frame")
	}
	if n > maxFrame {
		return false, 0, nil, fmt.Errorf("cdp: frame of %d bytes exceeds the 16MB limit", n)
	}
	p = make([]byte, n)
	_, err = io.ReadFull(w.br, p)
	return
}

// readMessage returns the next text message, reassembling fragments and answering ping/close.
func (w *wsConn) readMessage() ([]byte, error) {
	var msg []byte
	for {
		fin, op, p, err := w.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			_ = w.writeFrame(opPong, p)
		case opPong:
		case opClose:
			_ = w.writeFrame(opClose, p)
			return nil, io.EOF
		case opText, opCont:
			if msg = append(msg, p...); len(msg) > maxFrame {
				return nil, errors.New("cdp: message exceeds the 16MB limit")
			}
			if fin {
				return msg, nil
			}
		}
	}
}

func (w *wsConn) close() error {
	_ = w.writeFrame(opClose, nil)
	return w.c.Close()
}
