package launch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A vendor the harness cannot route still launches its own CLI untouched, after one configure offer.
func TestVendorPassesThroughWhenUnroutable(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("HARNESS_PROVIDER", "subscription") // passthrough auth: gemini cannot use it
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("HOME", t.TempDir()) // no vendor login state on disk
	fake := filepath.Join(t.TempDir(), "gemini")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := lookPath
	lookPath = func(name string) (string, error) {
		if name == "gemini" {
			return fake, nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { lookPath = orig })
	offers := 0
	OfferConfigure = func(vendor string, reason error) bool { offers++; return false }
	t.Cleanup(func() { OfferConfigure = nil })

	plan, err := Vendor(context.Background(), "gemini", Options{}, []string{"auth", "login"})
	if err != nil {
		t.Fatalf("expected a passthrough plan, got %v", err)
	}
	if plan.Binary != fake || len(plan.Argv) != 2 || plan.Argv[0] != "auth" || len(plan.Set) != 0 {
		t.Errorf("plan = %+v, want the plain CLI with the user's argv and no harness env", plan)
	}
	if offers != 1 {
		t.Errorf("configure offered %d times, want 1", offers)
	}
	// --print-env never prompts.
	offers = 0
	if _, err := Vendor(context.Background(), "gemini", Options{PrintEnv: true}, nil); err != nil || offers != 0 {
		t.Errorf("print-env: err=%v offers=%d", err, offers)
	}
}
