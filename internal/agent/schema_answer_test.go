package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestValidEnvNameRefusesScrubbedNames(t *testing.T) {
	for _, bad := range []string{"HARNESS_VAULT_KEY", "HARNESS_ANYTHING", "ANTHROPIC_API_KEY", "DATABRICKS_HOST", "CLAUDE_PROFILE", "FOO_TOKEN"} {
		if err := ValidEnvName(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	for _, ok := range []string{"HTTP_PROXY", "ANTHROPIC_BASE_URL", "FOO"} {
		if err := ValidEnvName(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	if err := ValidateEnv([]string{"sk-supersecret"}); err == nil || strings.Contains(err.Error(), "supersecret") {
		t.Errorf("a no-= entry must be refused without echoing it: %v", err)
	}
}

func TestStructuredAnswerNested(t *testing.T) {
	const schema = `{"type":"object","required":["a"],"properties":{"a":{"type":"object","required":["b"],` +
		`"properties":{"b":{"type":"string","enum":["x","y"]},"l":{"type":"array","items":{"type":"integer"}}}},"c":{"minimum":1}}}`
	for answer, wantErr := range map[string]bool{
		`{"a":{"b":"x","l":[1,2]}}`:   false,
		`{"a":{}}`:                    true, // nested required
		`{"a":{"b":"z"}}`:             true, // enum
		`{"a":{"b":"x","l":[1,"s"]}}`: true, // items type
	} {
		_, _, err := structuredAnswer(answer, schema)
		if (err != nil) != wantErr {
			t.Errorf("%s: err=%v, want error=%v", answer, err, wantErr)
		}
	}
	_, unchecked, _ := structuredAnswer(`{"a":{"b":"x"}}`, `{"type":"object","properties":{"n":{"minimum":1,"pattern":"x"}},"title":"t"}`)
	if !slices.Equal(unchecked, []string{"minimum", "pattern"}) {
		t.Errorf("unchecked = %v, want [minimum pattern]", unchecked)
	}
}

// The --no-tools HTTP one-shot spawns no process, so --env must be reported as dropped.
func TestNoToolsOneShotReportsEnvDropped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, anthropicSSE)
	}))
	defer srv.Close()
	registry(t, srv)
	res, err := Run(context.Background(), claudeRunner{}, Request{Prompt: "x", Provider: "tanth", Model: "haiku", NoTools: true, Env: []string{"FOO=1"}}, Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Dropped, []string{"--env"}) {
		t.Errorf("dropped = %v, want [--env]", res.Dropped)
	}
}

// Boolean schemas: false forbids any value at that spot, true allows any.
func TestStructuredAnswerBooleanSchemas(t *testing.T) {
	const schema = `{"type":"object","properties":{"x":false,"y":true,"l":{"type":"array","items":false}}}`
	for answer, wantErr := range map[string]bool{
		`{"y":{"any":1},"l":[]}`: false,
		`{"x":1}`:                true, // property false: must be absent
		`{"l":[1]}`:              true, // items false: no elements
	} {
		if _, _, err := structuredAnswer(answer, schema); (err != nil) != wantErr {
			t.Errorf("%s: err=%v, want error=%v", answer, err, wantErr)
		}
	}
	if _, _, err := structuredAnswer(`{}`, `false`); err == nil {
		t.Error("top-level false schema accepted an answer")
	}
}
