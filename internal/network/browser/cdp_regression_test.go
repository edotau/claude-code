package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExtractTemplateSameDocumentNavigation(t *testing.T) {
	page := 0
	base := fakeCDPFn(t, func(method string, params json.RawMessage) (string, bool) {
		switch method {
		case "Page.navigate":
			page++
			var p struct{ URL string }
			_ = json.Unmarshal(params, &p)
			if !strings.HasSuffix(p.URL, "#page="+itoa(page)) {
				t.Errorf("navigation %d: %s", page, p.URL)
			}
			if page == 1 {
				return `{"frameId":"F","loaderId":"L"}`, true
			}
			return `{"frameId":"F"}`, false
		case "Runtime.evaluate":
			return evalResult(rowsFor("page", page)), false
		}
		return `{}`, false
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var out bytes.Buffer
	err := Extract(ctx, Env{Sleep: func(time.Duration) {}}, ExtractOptions{
		URL: "https://example.test/items", Template: "https://example.test/items#page={page}",
		Container: "li", Fields: pipelineFields[:1], Format: "jsonl", MaxPages: 2,
		IgnoreRobots: true, CDPBase: base,
	}, &out)
	if err != nil {
		t.Fatalf("fragment template extraction: %v", err)
	}
	if page != 2 || out.String() != "{\"title\":\"page0\"}\n{\"title\":\"page0\"}\n{\"title\":\"page1\"}\n" {
		t.Fatalf("pages=%d output=%q", page, out.String())
	}
}

func TestCallCancellationCoversCommandWrite(t *testing.T) {
	for _, mode := range []string{"already canceled", "waiting for writer", "blocked socket write"} {
		t.Run(mode, func(t *testing.T) {
			assertCallWriteCancellation(t, mode)
		})
	}
}

func assertCallWriteCancellation(t *testing.T, mode string) {
	t.Helper()
	client, peer := net.Pipe()
	w := &wsConn{c: client, wsem: make(chan struct{}, 1)}
	locked := mode != "blocked socket write"
	if locked {
		w.wsem <- struct{}{}
	}
	c := &cdpConn{sock: w, pending: map[int]chan response{}, done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if mode == "already canceled" {
		cancel()
	}
	result, done := make(chan error, 1), make(chan struct{})
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.Close()
		if locked {
			<-w.wsem
		}
		<-done
	})
	go func() {
		defer close(done)
		_, err := c.call(ctx, "Runtime.evaluate", nil)
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("command write succeeded after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("command write outlived caller cancellation")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) != 0 {
		t.Fatalf("canceled write left %d pending commands", len(c.pending))
	}
}

func TestTabCloseFailuresReachCallers(t *testing.T) {
	original := httpClient
	httpClient = &http.Client{Timeout: 200 * time.Millisecond}
	t.Cleanup(func() { httpClient = original })
	for _, failure := range []string{"status", "timeout"} {
		for _, flow := range []string{"Close", "Extract"} {
			t.Run(failure+"/"+flow, func(t *testing.T) {
				assertTabCloseFailure(t, failure, flow)
			})
		}
	}
}

func assertTabCloseFailure(t *testing.T, failure, flow string) {
	t.Helper()
	closeTab := func(w http.ResponseWriter, r *http.Request) {
		if failure == "timeout" {
			<-r.Context().Done()
			return
		}
		http.Error(w, "tab could not close", http.StatusInternalServerError)
	}
	base := fakeCDPWithClose(t, func(method string, _ json.RawMessage) (string, bool) {
		if method == "Runtime.evaluate" {
			return evalResult(`[["row"]]`), false
		}
		return `{"loaderId":"L"}`, method == "Page.navigate"
	}, closeTab)
	var err error
	if flow == "Extract" {
		err = Extract(ctx5(t), Env{}, ExtractOptions{
			URL: "https://example.test/", CDPBase: base, IgnoreRobots: true,
			Container: "li", Fields: pipelineFields[:1], Format: "jsonl",
		}, io.Discard)
	} else {
		var c *cdpConn
		c, err = dialCDP(ctx5(t), base)
		if err != nil {
			t.Fatal(err)
		}
		err = c.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "close tab T") {
		t.Fatalf("%s hid tab-close %s: %v", flow, failure, err)
	}
	assertCloseFailureCause(t, failure, err)
}

func assertCloseFailureCause(t *testing.T, failure string, err error) {
	t.Helper()
	if failure == "timeout" {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("close timeout lost its cause: %v", err)
		}
	} else if !strings.Contains(err.Error(), "500 Internal Server Error") {
		t.Fatalf("close failure lost HTTP status: %v", err)
	}
}

func TestNoBrowserHintNamesNativeLaunchCommand(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("native Linux/WSL hint is not selected on macOS")
	}
	_, err := dialCDP(ctx5(t), closedBase(t))
	if err == nil || !strings.Contains(err.Error(), "start Chrome with --remote-debugging-port=9222") {
		t.Fatalf("missing native launch instruction: %v", err)
	}
}
