package browser

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/cdp"
)

// fakeCDP answers every Runtime.evaluate with the canned by-value JSON; Extract itself dials :9222 (no seam).
func fakeCDP(t *testing.T, value string) string {
	mux := http.NewServeMux()
	mux.HandleFunc("/json/new", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":"T","webSocketDebuggerUrl":"ws://`+r.Host+`/ws"}`)
	})
	mux.HandleFunc("/json/close/", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		c, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
		for {
			id, ok := readEvalID(brw.Reader)
			if !ok {
				return
			}
			body := `{"id":` + id + `,"result":{"result":{"type":"object","value":` + value + `}}}`
			out := []byte{0x81}
			if n := len(body); n < 126 {
				out = append(out, byte(n))
			} else {
				out = binary.BigEndian.AppendUint16(append(out, 126), uint16(n))
			}
			_, _ = c.Write(append(out, body...))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// readEvalID reads one masked client text frame and returns its command id.
func readEvalID(br *bufio.Reader) (string, bool) {
	var h [2]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		return "", false
	}
	n := int(h[1] & 0x7F)
	if n == 126 {
		var b [2]byte
		_, _ = io.ReadFull(br, b[:])
		n = int(binary.BigEndian.Uint16(b[:]))
	}
	var m [4]byte
	_, _ = io.ReadFull(br, m[:])
	p := make([]byte, n)
	if _, err := io.ReadFull(br, p); err != nil {
		return "", false
	}
	for i := range p {
		p[i] ^= m[i%4]
	}
	var cmd struct{ ID json.Number }
	if h[0]&0x0F != 1 || json.Unmarshal(p, &cmd) != nil {
		return "", h[0]&0x0F != 8
	}
	return cmd.ID.String(), true
}

func grabFrom(t *testing.T, value string) []Row {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := cdp.Dial(ctx, fakeCDP(t, value))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	rows, err := grab(ctx, c, "ignored")
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func str(s string) *string { return &s }

func TestExtractPipeline(t *testing.T) {
	// raw rows: dirty whitespace, zero-width chars, null selector, whitespace-only, duplicate key, null key twice
	rows := grabFrom(t, `[
 ["  Alpha \n\t Beta ", "/a"],
 ["Gam\u200bma\ufeff", null],
 ["   ", "/c"],
 ["Alpha Beta", "/dup"],
 [null, "/n1"],
 [null, "/n2"]]`)
	want := []Row{{str("Alpha Beta"), str("/a")}, {str("Gamma"), nil}, {nil, str("/c")},
		{str("Alpha Beta"), str("/dup")}, {nil, str("/n1")}, {nil, str("/n2")}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("cleaned rows = %s", dump(rows))
	}

	fields := []Field{{Name: "title", Selector: "h2"}, {Name: "link", Selector: "a", Attr: "href"}}
	deduped := dedupe(rows, 0)
	if len(deduped) != 5 || *deduped[0][1] != "/a" || deduped[3][1] == nil || *deduped[3][1] != "/n1" {
		t.Fatalf("dedupe(key=title) = %s; later duplicate dropped, null keys kept", dump(deduped))
	}
	if len(dedupe(rows, -1)) != 6 {
		t.Fatal("no key must keep every row")
	}

	var csvOut bytes.Buffer
	if err := writeRows(&csvOut, ExtractOptions{Fields: fields, Format: "csv"}, deduped[:2]); err != nil {
		t.Fatal(err)
	}
	if got := csvOut.String(); got != "title,link\nAlpha Beta,/a\nGamma,\n" {
		t.Fatalf("csv = %q", got)
	}

	var js bytes.Buffer
	if err := writeRows(&js, ExtractOptions{Fields: fields, Format: "json"}, []Row{{str("a&b"), nil}}); err != nil {
		t.Fatal(err)
	}
	var arr []map[string]*string
	if err := json.Unmarshal(js.Bytes(), &arr); err != nil || len(arr) != 1 || *arr[0]["title"] != "a&b" || arr[0]["link"] != nil {
		t.Fatalf("json = %q err=%v", js.String(), err)
	}
	if !strings.Contains(js.String(), "a&b") {
		t.Fatal("HTML escaping must stay off")
	}
	var empty bytes.Buffer
	_ = writeRows(&empty, ExtractOptions{Fields: fields, Format: "json"}, nil)
	if empty.String() != "[]\n" {
		t.Fatalf("empty json = %q", empty.String())
	}
	var jl bytes.Buffer
	_ = writeRows(&jl, ExtractOptions{Fields: fields, Format: "jsonl"}, deduped[:2])
	if jl.String() != "{\"title\":\"Alpha Beta\",\"link\":\"/a\"}\n{\"title\":\"Gamma\",\"link\":null}\n" {
		t.Fatalf("jsonl = %q", jl.String())
	}
}

func dump(rows []Row) string {
	var parts []string
	for _, r := range rows {
		b, _ := json.Marshal(r)
		parts = append(parts, string(b))
	}
	return strings.Join(parts, " ")
}

func TestExtractRefusedByRobotsBeforeAnyBrowserWork(t *testing.T) {
	srv := robotsServer(t, 200, "User-agent: claude-code\nDisallow: /secret\n", nil)
	err := Extract(context.Background(), Env{}, ExtractOptions{
		URL: srv.URL + "/secret/list", Container: "li", Fields: []Field{{Name: "t", Selector: "a"}}, Format: "jsonl",
	}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "Disallow: /secret") {
		t.Fatalf("err = %v, want robots refusal naming the rule", err)
	}
}

func TestParseField(t *testing.T) {
	ok := map[string]Field{
		"title=h2.t":            {"title", "h2.t", ""},
		" link = a.x@href ":     {"link", "a.x", "href"},
		"img=img@data-src":      {"img", "img", "data-src"},
		"mail=a[href^=mailto:]": {"mail", "a[href^=mailto:]", ""},
		"x=a[title=\"q@z\"]":    {"x", "a[title=\"q@z\"]", ""},
	}
	for in, want := range ok {
		got, err := ParseField(in)
		if err != nil || got != want {
			t.Errorf("ParseField(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "novalue", "=sel", "name=", "  =  "} {
		if _, err := ParseField(bad); err == nil {
			t.Errorf("ParseField(%q) should fail", bad)
		}
	}
}

func TestExtractValidate(t *testing.T) {
	good := ExtractOptions{URL: "https://e.test/", Container: "li", Fields: []Field{{Name: "a", Selector: "x"}}, Format: "jsonl"}
	if err := good.Validate(); err != nil {
		t.Fatalf("good options rejected: %v", err)
	}
	mut := func(f func(*ExtractOptions)) ExtractOptions { o := good; f(&o); return o }
	bad := map[string]ExtractOptions{
		"next+scroll":      mut(func(o *ExtractOptions) { o.Next, o.Scroll = "a.n", true }),
		"next+pages":       mut(func(o *ExtractOptions) { o.Next, o.Template = "a.n", "https://e.test/?p={page}" }),
		"load-more+scroll": mut(func(o *ExtractOptions) { o.LoadMore, o.Scroll = "button", true }),
		"pages no {page}":  mut(func(o *ExtractOptions) { o.Template = "https://e.test/?p=1" }),
		"no container":     mut(func(o *ExtractOptions) { o.Container = "" }),
		"no fields":        mut(func(o *ExtractOptions) { o.Fields = nil }),
		"dup field":        mut(func(o *ExtractOptions) { o.Fields = append(o.Fields, o.Fields[0]) }),
		"unknown key":      mut(func(o *ExtractOptions) { o.Key = "zzz" }),
		"bad format":       mut(func(o *ExtractOptions) { o.Format = "xml" }),
	}
	for name, o := range bad {
		if err := o.Validate(); err == nil {
			t.Errorf("%s: Validate should fail", name)
		}
	}
	// non-http(s) schemes are refused by the gate that Extract runs first, even with --ignore-robots
	for _, u := range []string{"file:///etc/passwd", "javascript:alert(1)", "ftp://h/x", "data:text/html,hi"} {
		o := good
		o.URL, o.IgnoreRobots = u, true
		if err := Extract(context.Background(), Env{}, o, io.Discard); err == nil || !strings.Contains(err.Error(), "http") {
			t.Errorf("Extract(%q) err = %v, want http(s)-only refusal", u, err)
		}
	}
}
