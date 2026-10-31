package browser

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBrowser serves /json/new, /json/close/<id> and a tiny websocket the handler drives.
type fakeBrowser struct {
	t       *testing.T
	srv     *httptest.Server
	handle  func(f *fakeBrowser, id int, method string, params json.RawMessage)
	mu      sync.Mutex
	closed  []string
	masked  []bool // mask bit of every client frame
	pongs   int
	conn    interface{ Write([]byte) (int, error) }
	writeMu sync.Mutex
	wsDone  chan struct{} // closed when serveWS returns, so every client frame is counted
}

func newFakeBrowser(t *testing.T, h func(f *fakeBrowser, id int, method string, params json.RawMessage)) *fakeBrowser {
	f := &fakeBrowser{t: t, handle: h, wsDone: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/new", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "PUT only", http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.WriteString(w, `{"id":"TAB1","webSocketDebuggerUrl":"ws://`+r.Host+`/ws"}`)
	})
	mux.HandleFunc("/json/close/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.closed = append(f.closed, strings.TrimPrefix(r.URL.Path, "/json/close/"))
		f.mu.Unlock()
		_, _ = io.WriteString(w, "Target is closing")
	})
	mux.HandleFunc("/ws", f.serveWS)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBrowser) serveWS(w http.ResponseWriter, r *http.Request) {
	defer close(f.wsDone)
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + wsGUID))
	c, brw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()
	_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
	f.conn = c
	br := brw.Reader
	for {
		op, p, masked, err := readClientFrame(br)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.masked = append(f.masked, masked)
		f.mu.Unlock()
		switch op {
		case opClose:
			return
		case opPong:
			f.mu.Lock()
			f.pongs++
			f.mu.Unlock()
		case opText:
			var m struct {
				ID     int
				Method string
				Params json.RawMessage
			}
			if json.Unmarshal(p, &m) == nil {
				f.handle(f, m.ID, m.Method, m.Params)
			}
		}
	}
}

func readClientFrame(br *bufio.Reader) (op byte, p []byte, masked bool, err error) {
	var h [2]byte
	if _, err = io.ReadFull(br, h[:]); err != nil {
		return
	}
	op, masked = h[0]&0x0F, h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		_, err = io.ReadFull(br, b[:])
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		_, err = io.ReadFull(br, b[:])
		n = binary.BigEndian.Uint64(b[:])
	}
	var mask [4]byte
	if masked {
		_, err = io.ReadFull(br, mask[:])
	}
	if err != nil {
		return
	}
	p = make([]byte, n)
	if _, err = io.ReadFull(br, p); err != nil {
		return
	}
	for i := range p {
		p[i] ^= mask[i%4]
	}
	return
}

// frame builds an unmasked server frame (fin and opcode explicit).
func frame(fin bool, op byte, p []byte) []byte {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	out := []byte{b0}
	switch n := len(p); {
	case n < 126:
		out = append(out, byte(n))
	case n < 1<<16:
		out = append(out, 126, byte(n>>8), byte(n))
	default:
		out = binary.BigEndian.AppendUint64(append(out, 127), uint64(n))
	}
	return append(out, p...)
}

func (f *fakeBrowser) send(b []byte) {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_, _ = f.conn.Write(b)
}

func (f *fakeBrowser) reply(id int, result string) {
	f.send(frame(true, opText, []byte(`{"id":`+itoa(id)+`,"result":`+result+`}`)))
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func ctx5(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// Spans dialCDP -> Navigate (event wait) -> Eval (fragmented reply + ping) -> Close (tab closed over HTTP).
func TestRoundTrip(t *testing.T) {
	var navURL string
	f := newFakeBrowser(t, func(f *fakeBrowser, id int, method string, params json.RawMessage) {
		switch method {
		case "Page.navigate":
			var p struct{ URL string }
			_ = json.Unmarshal(params, &p)
			navURL = p.URL
			f.reply(id, `{"frameId":"F"}`)
			f.send(frame(true, opText, []byte(`{"method":"Page.loadEventFired","params":{"timestamp":1}}`)))
		case "Runtime.evaluate":
			full := []byte(`{"id":` + itoa(id) + `,"result":{"result":{"type":"object","value":[["a",null],["b","c"]]}}}`)
			f.send(frame(true, opPing, []byte("hi")))
			f.send(frame(false, opText, full[:20]))
			f.send(frame(true, opCont, full[20:]))
		default:
			f.reply(id, `{}`)
		}
	})
	c, err := dialCDP(ctx5(t), f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Navigate(ctx5(t), "https://example.test/x"); err != nil {
		t.Fatal(err)
	}
	if navURL != "https://example.test/x" {
		t.Fatalf("navigate url = %q", navURL)
	}
	var out [][]*string
	if err := c.Eval(ctx5(t), "1+1", &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0][1] != nil || *out[1][1] != "c" {
		t.Fatalf("eval out = %v", out)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.wsDone:
	case <-ctx5(t).Done():
		t.Fatal("fake browser never saw the socket close")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.closed) != 1 || f.closed[0] != "TAB1" {
		t.Fatalf("closed tabs = %v, want [TAB1]", f.closed)
	}
	if f.pongs != 1 {
		t.Fatalf("pongs = %d, want 1 (ping must be answered)", f.pongs)
	}
	if len(f.masked) < 4 {
		t.Fatalf("only %d client frames seen", len(f.masked))
	}
	for i, m := range f.masked {
		if !m {
			t.Fatalf("client frame %d unmasked", i)
		}
	}
}

func TestEvalExceptionIsError(t *testing.T) {
	f := newFakeBrowser(t, func(f *fakeBrowser, id int, method string, _ json.RawMessage) {
		f.reply(id, `{"result":{"type":"object"},"exceptionDetails":{"text":"Uncaught","exception":{"description":"ReferenceError: nope is not defined"}}}`)
	})
	c, err := dialCDP(ctx5(t), f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	err = c.Eval(ctx5(t), "nope", nil)
	if err == nil || !strings.Contains(err.Error(), "ReferenceError: nope") {
		t.Fatalf("err = %v, want the exception description", err)
	}
}

func TestProtocolError(t *testing.T) {
	f := newFakeBrowser(t, func(f *fakeBrowser, id int, _ string, _ json.RawMessage) {
		f.send(frame(true, opText, []byte(`{"id":`+itoa(id)+`,"error":{"message":"bad things"}}`)))
	})
	c, err := dialCDP(ctx5(t), f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Eval(ctx5(t), "1", nil); err == nil || !strings.Contains(err.Error(), "bad things") {
		t.Fatalf("err = %v", err)
	}
}

func TestHostileServerFramesRejected(t *testing.T) {
	cases := map[string][]byte{
		"masked server frame": append([]byte{0x81, 0x80 | 2, 1, 2, 3, 4}, 'h', 'i'),
		"over 16MB header":    append([]byte{0x81, 127}, binary.BigEndian.AppendUint64(nil, 17<<20)...),
	}
	for name, evil := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeBrowser(t, func(f *fakeBrowser, _ int, _ string, _ json.RawMessage) { f.send(evil) })
			c, err := dialCDP(ctx5(t), f.srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			err = c.Eval(ctx5(t), "1", nil)
			if err == nil || !strings.Contains(err.Error(), "connection closed") {
				t.Fatalf("err = %v, want connection closed", err)
			}
			if !strings.Contains(c.err.Error(), map[string]string{"masked server frame": "masked", "over 16MB header": "16MB"}[name]) {
				t.Fatalf("read error = %v", c.err)
			}
		})
	}
}

func TestDialNoBrowser(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close()
	if _, err := dialCDP(ctx5(t), base); err == nil || !strings.Contains(err.Error(), "no browser answers") {
		t.Fatalf("err = %v, want errNoCDP", err)
	}
}
