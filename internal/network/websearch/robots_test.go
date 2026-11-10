package websearch

import (
	"strings"
	"testing"
	"time"
)

func TestRobotsDisallowsLongestMatch(t *testing.T) {
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
	rules := parseRobots(txt)
	for _, tc := range cases {
		r, blocked := robotsDisallows(rules, tc.path)
		if blocked != tc.blocked || (blocked && r.pattern != tc.rule) {
			t.Errorf("%s: blocked=%v rule=%q, want %v %q", tc.path, blocked, r.pattern, tc.blocked, tc.rule)
		}
	}
}

func TestParseRobotsEmptyDisallowIsAllowAllGroup(t *testing.T) {
	cases := []struct {
		name, txt string
		blocked   bool
	}{
		{"claude-code allow-all beats * Disallow /", "User-agent: claude-code\nDisallow:\n\nUser-agent: *\nDisallow: /\n",
			false},
		{"claude-code Disallow /x applies", "User-agent: claude-code\nDisallow: /x\n\nUser-agent: *\nDisallow:\n", true},
		{"only * Disallow / applies", "User-agent: *\nDisallow: /\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, blocked := robotsDisallows(parseRobots(c.txt), "/x/page")
			if blocked != c.blocked {
				t.Errorf("blocked = %v, want %v", blocked, c.blocked)
			}
		})
	}
}

func TestRobotsMatchMultiWildcardIsBounded(t *testing.T) {
	pat := "/" + strings.Repeat("*a", 10) + "*b"
	path := "/" + strings.Repeat("a", 60)
	done := make(chan bool, 1)
	go func() { done <- robotsMatch(pat, path) }()
	select {
	case got := <-done:
		if got {
			t.Errorf("robotsMatch(%q) matched a path without b", pat)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("robotsMatch did not finish: exponential backtracking")
	}
	if !robotsMatch(pat, path+"b") {
		t.Errorf("robotsMatch(%q) must match a path ending in b", pat)
	}
	if robotsMatch("/a*b$", "/aXbY") || !robotsMatch("/a*b$", "/aXb") {
		t.Errorf("trailing $ must anchor the last segment")
	}
}

func TestParseRobotsStripsLeadingBOM(t *testing.T) {
	_, blocked := robotsDisallows(parseRobots("\ufeffUser-agent: *\nDisallow: /\n"), "/x/page")
	if !blocked {
		t.Error("BOM-prefixed first group was ignored; want Disallow / to apply")
	}
}
