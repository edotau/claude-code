package providers

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestCodexCachedModelsHonoursCodexHome: models_cache.json is read from $CODEX_HOME, not always ~/.codex.
func TestCodexCachedModelsHonoursCodexHome(t *testing.T) {
	codex := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", codex)
	cache := `{"models":[{"slug":"gpt-x","display_name":"X"}]}`
	if err := os.WriteFile(filepath.Join(codex, "models_cache.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := codexCachedModels()
	if err != nil || len(got) != 1 || got[0].ID != "gpt-x" {
		t.Fatalf("codexCachedModels = %+v, %v; want [gpt-x]", got, err)
	}
}

// TestHTTPClientHasTimeout: a hung endpoint must not block forever, yet DefaultClient's Transport seam is kept.
func TestHTTPClientHasTimeout(t *testing.T) {
	c := httpClient()
	if c.Timeout <= 0 {
		t.Fatalf("httpClient().Timeout = %v; want > 0", c.Timeout)
	}
	if c.Transport != http.DefaultClient.Transport {
		t.Fatal("httpClient must keep http.DefaultClient's Transport")
	}
}

// Given no codex cache, the returned error wraps the live-fetch error so errors.Is still matches.
func TestFallbackModelsCodexWrapsLiveErr(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	live := errors.New("live boom")
	p := &Provider{Name: "chatgpt", Provider: "openai", Auth: Auth{Login: "codex"}}
	_, _, err := FallbackModels(p, live)
	if !errors.Is(err, live) {
		t.Errorf("err = %v; want it to wrap the live error", err)
	}
}

func TestDemote(t *testing.T) {
	p := &Provider{Name: "g", Models: map[string]string{"opus": "big", "sonnet": "mid", "haiku": "small", "fable": "other"}}
	if d, ok := Demote(p, "big"); !ok || d.Model != "mid" {
		t.Errorf("Demote(big) = %v %v, want mid", d.Model, ok)
	}
	if d, ok := Demote(p, "mid"); !ok || d.Model != "small" {
		t.Errorf("Demote(mid) = %v %v, want small", d.Model, ok)
	}
	if _, ok := Demote(p, "small"); ok {
		t.Error("Demote(small) must stop at the bottom of the ladder")
	}
	if _, ok := Demote(&Provider{Name: "one", Models: map[string]string{"opus": "x", "sonnet": "x"}}, "x"); ok {
		t.Error("Demote must not return the same model under another alias")
	}
}

// IsQuota is any account-exhaustion or model-denial marker; ModelDenied is a model marker no hard account marker vetoes.
func TestQuotaClassification(t *testing.T) {
	for _, c := range []struct {
		text          string
		quota, denied bool
	}{
		{"usage limit reached, try again in 60 seconds", true, false},
		{"HTTP 402 payment required", true, false},
		{"HTTP 402", true, false},
		{"payment required", true, false},
		{"you have exceeded your monthly quota", true, false},
		{"exceeded your weekly quota", true, false},
		{"exceeded your current limit", true, false},
		{"out of premium requests", true, false},
		{"limit: 0", true, true},
		{"insufficient_quota", true, true},
		{"quota exceeded for metric: generate_content_free_tier_requests", true, true},
		{"exceeded your current quota", true, true},
		{"exceeded your current quota, limit: 0", true, true}, // gemini free tier: model-denied, not account-wide
		{"HTTP 402 payment required: insufficient_quota", true, false},
		{"out of credits", true, false},
		{"status 402", true, false},
		{"status=402", true, false},
		{"code: 402", true, false},
		{"read tcp 10.0.0.1:402: connection reset", false, false},
		{"stream ended after 402 bytes limit: 0", true, true}, // a bare 402 does not veto
		{"You've hit your limit · resets 3pm", true, false},
		{"Out of usage credits", true, false},
		{"You've hit your channel's monthly spend limit.", true, false},
		{"You've hit your fast limit", false, false},
		{"429 Too Many Requests, retry in 2s", false, false},
		{"rate limit exceeded", false, false},
		{"line 4021", false, false},
	} {
		if IsQuota(c.text) != c.quota || ModelDenied(c.text) != c.denied {
			t.Errorf("%q: IsQuota = %v, ModelDenied = %v; want %v, %v", c.text, IsQuota(c.text), ModelDenied(c.text), c.quota, c.denied)
		}
	}
}
