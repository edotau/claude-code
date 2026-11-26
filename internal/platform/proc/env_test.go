package proc

import (
	"os"
	"testing"
)

func TestSetenvUnsetenv(t *testing.T) {
	const key = "PROC_ENV_WRAPPER_TEST"
	t.Setenv(key, "")
	if err := Setenv(key, "v"); err != nil || os.Getenv(key) != "v" {
		t.Fatalf("Setenv: err=%v value=%q", err, os.Getenv(key))
	}
	if err := Unsetenv(key); err != nil {
		t.Fatalf("Unsetenv: %v", err)
	}
	if _, ok := os.LookupEnv(key); ok {
		t.Fatal("key still set after Unsetenv")
	}
}
