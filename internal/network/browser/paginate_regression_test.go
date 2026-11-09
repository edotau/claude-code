package browser

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWaitChangeDetectsLaterRowText(t *testing.T) {
	expr := "(" + sigJS("li") + ")"
	before := pageSignature(t, expr, []string{"unchanged first row", "old second row"})
	after := pageSignature(t, expr, []string{"unchanged first row", "new second row"})
	c, err := dialCDP(ctx5(t), fakeCDP(t, jsString(after)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	env := Env{Sleep: func(time.Duration) {}}
	changed, err := waitChange(ctx5(t), env, c, expr, before)
	if err != nil || !changed {
		t.Fatalf("later row changed at the same URL and count: changed=%v err=%v", changed, err)
	}
	changed, err = waitChange(ctx5(t), env, c, expr, after)
	if err != nil || changed {
		t.Fatalf("unchanged rows: changed=%v err=%v", changed, err)
	}
}

func pageSignature(t *testing.T, expr string, rows []string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("JavaScript signature regression requires Node.js")
	}
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	script := `const rows=` + string(data) + `;
const location={href:'https://example.test/items'};
const document={querySelectorAll:()=>rows.map(textContent=>({textContent})),
querySelector:()=>({textContent:rows[0]})};
process.stdout.write(String(eval(` + jsString(expr) + `)));`
	out, err := exec.CommandContext(ctx5(t), node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("evaluate generated signature: %v: %s", err, out)
	}
	return string(out)
}

type slowLoadMore struct{ clicks, probes, count int }

func (s *slowLoadMore) handle(method string, params json.RawMessage) (string, bool) {
	var p struct{ Expression string }
	_ = json.Unmarshal(params, &p)
	switch {
	case method != "Runtime.evaluate":
		return `{}`, false
	case strings.Contains(p.Expression, "b.click()"):
		s.probes++
		if s.clicks == 2 {
			return evalResult(`false`), false
		}
		s.clicks++
		if s.clicks == 2 {
			s.count = 2
		}
		return evalResult(`true`), false
	case p.Expression == "rows":
		return evalResult(rowsFor("row", s.count)), false
	default:
		return evalResult(itoa(s.count)), false
	}
}

func TestLoadMoreContinuesAfterSlowResponse(t *testing.T) {
	s := &slowLoadMore{count: 1}
	c, err := dialCDP(ctx5(t), fakeCDPFn(t, s.handle))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	rows, err := growPage(ctx5(t), Env{Sleep: func(time.Duration) {}}, c,
		ExtractOptions{Container: "li", LoadMore: "button.more", MaxPages: 5}, "rows")
	if err != nil || len(rows) != 2 || s.clicks != 2 || s.probes != 3 {
		t.Fatalf("slow load-more: rows=%s clicks=%d probes=%d err=%v", dump(rows), s.clicks, s.probes, err)
	}
}

func TestClickPaginationChecksLandedRobots(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		t.Run(strconv.FormatBool(ignore), func(t *testing.T) {
			assertClickPaginationRobots(t, ignore)
		})
	}
}

func assertClickPaginationRobots(t *testing.T, ignore bool) {
	t.Helper()
	origin := robotsServer(t, 200, "User-agent: claude-code\nDisallow: /secret\n", nil).URL
	landed := origin + "/public"
	base := fakeCDPFn(t, func(method string, params json.RawMessage) (string, bool) {
		return clickPageReply(method, params, origin, &landed)
	})
	var out bytes.Buffer
	err := Extract(ctx5(t), Env{Sleep: func(time.Duration) {}}, ExtractOptions{
		URL: landed, CDPBase: base, Container: "li", Fields: pipelineFields[:1], Format: "jsonl",
		Next: "button.next", MaxPages: 2, IgnoreRobots: ignore,
	}, &out)
	if ignore {
		if err != nil || !strings.Contains(out.String(), "/secret") {
			t.Fatalf("--ignore-robots: output=%q err=%v", out.String(), err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "Disallow: /secret") {
		t.Fatalf("click bypassed robots: output=%q err=%v", out.String(), err)
	}
	if strings.Contains(out.String(), "/secret") {
		t.Fatalf("extracted disallowed rows: %q", out.String())
	}
}

func clickPageReply(method string, params json.RawMessage, origin string, landed *string) (string, bool) {
	if method != "Runtime.evaluate" {
		return `{"loaderId":"L"}`, method == "Page.navigate"
	}
	var p struct{ Expression string }
	_ = json.Unmarshal(params, &p)
	switch {
	case strings.Contains(p.Expression, "cfg.C"):
		return evalResult("[[" + jsString(*landed) + "]]"), false
	case strings.Contains(p.Expression, "state:'gone'"):
		return evalResult(`{"state":"ok","href":"","sig":"before"}`), false
	case strings.Contains(p.Expression, ".click()"):
		*landed = origin + "/secret"
		return evalResult(`null`), false
	case p.Expression == "location.href":
		return evalResult(jsString(*landed)), false
	default:
		return evalResult(`"after"`), false
	}
}
