package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func robotsServer(t *testing.T, status int, body string, hits *int) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		if hits != nil {
			*hits++
		}
		if ua := r.Header.Get("User-Agent"); ua != robotsAgent {
			t.Errorf("robots fetched with User-Agent %q", ua)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRobotsGate(t *testing.T) {
	const txt = `# comment
User-agent: *
Disallow: /star-private
Disallow: /

User-agent: claude-code
Disallow: /private
Disallow: /*.json$
Disallow: /search?q=
Allow: /private/public
Disallow: /tie
Allow: /tie
Disallow: /fix$
`
	cases := []struct {
		path    string
		blocked bool
		rule    string
	}{
		{"/", false, ""},                       // claude-code group beats the blanket * Disallow: /
		{"/star-private", false, ""},           // * group rules are ignored entirely
		{"/private", true, "/private"},         // prefix
		{"/private/x", true, "/private"},       // prefix covers children
		{"/private/public/a", false, ""},       // longer Allow beats shorter Disallow
		{"/data/x.json", true, "/*.json$"},     // wildcard + anchor
		{"/data/x.json?a=1", false, ""},        // $ anchors the end, query defeats it
		{"/data/x.jsonl", false, ""},           // $ again
		{"/search?q=cats", true, "/search?q="}, // query is part of the match path
		{"/tie", false, ""},                    // equal length: Allow wins
		{"/fix", true, "/fix$"},                // exact via $
		{"/fixed", false, ""},                  // $ blocks prefix-extension
		{"/other", false, ""},
	}
	srv := robotsServer(t, 200, txt, nil)
	for _, tc := range cases {
		err := newRobotsGate(false).check(context.Background(), srv.URL+tc.path)
		if tc.blocked != (err != nil) {
			t.Errorf("%s: blocked=%v err=%v", tc.path, tc.blocked, err)
			continue
		}
		if err != nil {
			for _, want := range []string{`Disallow: ` + tc.rule, "--ignore-robots", srv.URL} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: message %q lacks %q", tc.path, err, want)
				}
			}
		}
	}
}

func TestRobotsStarGroupFallbackAndAllowed(t *testing.T) {
	star := robotsServer(t, 200, "User-agent: *\nDisallow: /nope\n", nil)
	if err := newRobotsGate(false).check(context.Background(), star.URL+"/nope/x"); err == nil {
		t.Error("* group should apply when no claude-code group exists")
	}
	if err := newRobotsGate(true).check(context.Background(), star.URL+"/nope"); err != nil {
		t.Errorf("ignore=true must allow: %v", err)
	}
	if err := newRobotsGate(true).check(context.Background(), "ftp://x/y"); err == nil {
		t.Error("non-http(s) must be rejected even with ignore")
	}
}

func TestRobotsMissingOrUnreachableIsAllowed(t *testing.T) {
	nf := robotsServer(t, 404, "User-agent: *\nDisallow: /\n", nil)
	if err := newRobotsGate(false).check(context.Background(), nf.URL+"/x"); err != nil {
		t.Errorf("404 robots must allow: %v", err)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	u := dead.URL
	dead.Close()
	if err := newRobotsGate(false).check(context.Background(), u+"/x"); err != nil {
		t.Errorf("unreachable robots must allow: %v", err)
	}
}

func TestRobotsCachedPerOrigin(t *testing.T) {
	hits := 0
	srv := robotsServer(t, 200, "User-agent: *\nDisallow: /a\n", &hits)
	g := newRobotsGate(false)
	for _, p := range []string{"/a", "/b", "/c"} {
		_ = g.check(context.Background(), srv.URL+p)
	}
	if hits != 1 {
		t.Fatalf("robots.txt fetched %d times, want 1", hits)
	}
}
