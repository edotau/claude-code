package egress

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// socksServer is a CONNECT-only, no-auth SOCKS5 server on a loopback port; it stands in for `ssh -D`.
type socksServer struct {
	l        net.Listener
	connects atomic.Int32
}

func startSocks(t *testing.T, port int) *socksServer {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	s := &socksServer{l: l}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

// serve answers the greeting, then relays one CONNECT; a probe that only greets ends at the failed read.
func (s *socksServer) serve(conn net.Conn) {
	defer conn.Close()
	head := make([]byte, 3)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 5 {
		return
	}
	_, _ = conn.Write([]byte{5, 0})
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil || req[1] != 1 {
		return
	}
	size := map[byte]int{1: 4, 4: 16}[req[3]]
	if req[3] == 3 { // a domain name: one length byte, then the name
		n := make([]byte, 1)
		if _, err := io.ReadFull(conn, n); err != nil {
			return
		}
		size = int(n[0])
	}
	addr := make([]byte, size+2)
	if _, err := io.ReadFull(conn, addr); err != nil {
		return
	}
	host := string(addr[:size])
	if req[3] != 3 {
		host = net.IP(addr[:size]).String()
	}
	port := int(addr[size])<<8 | int(addr[size+1])
	target, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return
	}
	defer target.Close()
	s.connects.Add(1)
	_, _ = conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	go func() { _, _ = io.Copy(target, conn) }()
	_, _ = io.Copy(conn, target)
}

// A workspace request leaves through the host's tunnel; once the tunnel is gone it fails, never going direct.
func TestWorkspaceRequestsLeaveThroughTheTunnelOrFail(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	var hits atomic.Int32
	workspace := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(workspace.Close)
	var socks *socksServer
	origWorkspace, origSpawn, origWait := isWorkspace, spawn, startWait
	t.Cleanup(func() { isWorkspace, spawn, startWait = origWorkspace, origSpawn, origWait })
	isWorkspace = func(h string) bool { return h == "127.0.0.1" }
	startWait = 2 * time.Second
	spawn = func(_ string, port int, _ string) (int, <-chan struct{}, error) {
		socks = startSocks(t, port)
		return 0, nil, nil
	}
	tr := &http.Transport{Proxy: ProxyFor("box"), DisableKeepAlives: true}
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	resp, err := client.Get(workspace.URL)
	if err != nil {
		t.Fatalf("request through the tunnel: %v", err)
	}
	_ = resp.Body.Close()
	if socks == nil || socks.connects.Load() != 1 || hits.Load() != 1 {
		t.Fatalf("tunnel connects and workspace hits must both be 1; socks=%v hits=%d", socks, hits.Load())
	}

	_ = socks.l.Close()
	spawn = func(string, int, string) (int, <-chan struct{}, error) { return 0, nil, errors.New("ssh is gone") }
	resp, err = client.Get(workspace.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("request succeeded with the tunnel down")
	}
	if !strings.Contains(err.Error(), "egress host box: ssh is gone") || hits.Load() != 1 {
		t.Fatalf("err = %v, workspace hits = %d; want the egress host error and no direct request", err, hits.Load())
	}
}
