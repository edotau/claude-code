package assets

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"
)

func inventoryBytesMatch(reader func() []byte, kind, name string, want []byte) bool {
	if reader == nil {
		return kind == "template" && templateFixture(name)
	}
	return bytes.Equal(reader(), want)
}

func templateFixture(name string) bool {
	fix := BenchFixInput{Workdir: "/tmp/owner's lane", Package: "./internal/shell", OwnedLine: `["x"]`,
		Benchmark: "Bench", Cause: "cause", Numbers: "numbers", BeforeFile: "before", BenchCommand: "bench",
		IdentityCommand: "identity", Verify: "verify"}
	review := BenchReviewInput{Workdir: fix.Workdir, Package: fix.Package, OwnedLine: fix.OwnedLine,
		Benchmark: fix.Benchmark, Numbers: fix.Numbers, BeforeFile: fix.BeforeFile, AfterFile: "after",
		IdentityCommand: fix.IdentityCommand, Verify: fix.Verify, Diff: "- old\n+ new"}
	input := AutofixInput{Sha: "abc", Lane: "lane", Live: "live", Gate: "gate", Owned: `["internal/a.go"]`,
		Fence: "AUTOFIX-DATA-00000000000000000000000000000000", Judgment: []AutofixFinding{
			{Class: "complexity", Path: "internal/a.go", Line: 3, Rule: "rule", Message: "hostile text"}}}
	var out, hash string
	var err error
	switch name {
	case "BenchFixBrief":
		out, err = BenchFixBrief(fix)
		hash = "b2f3e25aa45acad15bd8960da89d837b6abc451a34183146a9028f0a612927f0"
	case "BenchReviewBrief":
		out, err = BenchReviewBrief(review)
		hash = "2283e17c3fe8d6aa3f658f75438f4910ce7062e1a6551b2e37be02b1fbf31c48"
	case "AutofixBrief":
		out, err = AutofixBrief(input)
		hash = "f1c94743080bcff2994d195eccc7db2f498f8297d6bf70835cded8cd92ae6365"
	default:
		return false
	}
	return err == nil && fmt.Sprintf("%x", sha256.Sum256([]byte(out))) == hash
}

func TestReviewTextParity(t *testing.T) {
	for _, tc := range []struct{ name, text, hash string }{
		{"no-diff", BenchNoDiff(), "c426ef7544973a056736c5ca8cbc002a959eefb2d2337c2d9d1c87b22bf9912a"},
		{"search", WebSearchDescription(), "461bf85e0d5b81769b06da9951c8e33adc3f12ec418bb1c091f54ebf140477cb"},
		{"extract", WebExtractDescription(), "da92d01bb59eaa42c944c610ae1316431c577cafc43b3fb821dde5de42c79dc9"},
		{"query", WebSearchQueryDescription(), "5a8dcbb363559abdab35bb51f423b1e8950cf7b282b4f3715237adbc133272c1"},
		{"max-results", WebSearchMaxResultsDescription(), "492e66b2cb5d19205bad1b2f2cbd5b4217f190f82d0ff8a6208941be311899e1"},
		{"urls", WebExtractURLsDescription(), "c80f8609ac3660a340522be49006044262a66592473ef24f15346b5bc020d915"},
		{"proxy-search", ProxySearchDescription(), "fc726384c5f7ba3a1e05317d1ba68fa9a224306707ac42e949d892afdca0dcd6"},
		{"proxy-schema", ProxySearchSchema(), "502d617adc30ea4d0ea29bff44f21c0387a799ae9c9e11d209e9ccdde22c65b5"},
		{"smoke", DatabricksSmokePrompt(), "d8adb66f9915732364edb6bad6a36db633ed580ae32a4edfa75d17ae3c1801e2"},
		{"probe", DatabricksProbePrompt(), "43ec2ed7e4a09a05579219ce12af8d8dd4093f98ea82fc37ec4ee4dbea66b978"},
		{"probe-tool", DatabricksToolDescription(), "279fe2199360d960bb449f9984a88e59232dc27c6b5c7bfb301e62aa7e23d423"},
		{"preflight", PreflightPongPrompt(), "ad8f50dfc4f073fb4749b44cd0a858295e6fd67a4f87632552b5877676524bb3"},
	} {
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(tc.text))); got != tc.hash {
			t.Errorf("%s: sha256 %s, want %s", tc.name, got, tc.hash)
		}
	}
}
