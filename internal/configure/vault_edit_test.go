package configure

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestHelperEditor(t *testing.T) {
	mode := os.Getenv("CONFIG_TEST_EDITOR")
	if mode == "" {
		return
	}
	path := os.Args[len(os.Args)-1]
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("editor file mode = %04o, want 0600", info.Mode().Perm())
	}
	if err := os.WriteFile(os.Getenv("CONFIG_TEST_EDITOR_SEEN"), []byte("seen"), 0600); err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "noop":
		os.Exit(0)
	case "fail":
		os.Exit(3)
	case "retry":
		marker := os.Getenv("CONFIG_TEST_EDITOR_RETRY")
		if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(marker, []byte("retry"), 0600); err != nil {
				t.Fatal(err)
			}
			mode = "bad"
		} else if err != nil {
			t.Fatal(err)
		} else {
			mode = "pin"
		}
	case "race":
		if err := vault.Update(func(tx *vault.Tx) error {
			return tx.Put(vault.Pins, "HARNESS_OPUS_MODEL", "concurrent-model")
		}); err != nil {
			t.Fatal(err)
		}
	case "pin", "bad":
	default:
		t.Fatalf("unknown helper editor mode %q", mode)
	}
	raw := []byte("{")
	if mode != "bad" {
		// A retry must replace the invalid file rather than try to decode it.
		d := Doc{Pins: map[string]string{"HARNESS_PROVIDER": "openai"}, Env: map[string]string{}}
		raw, err = json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func setupTestEditor(t *testing.T, mode, env string, terminal bool, answer bool) (string, *int) {
	t.Helper()
	vaulttest.Use(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	t.Setenv("CONFIG_TEST_EDITOR", mode)
	t.Setenv("CONFIG_TEST_EDITOR_SEEN", filepath.Join(tmp, "seen"))
	t.Setenv("CONFIG_TEST_EDITOR_RETRY", filepath.Join(tmp, "retry"))
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
	t.Setenv(env, os.Args[0]+" -test.run=^TestHelperEditor$ --")
	if env == "VISUAL" {
		t.Setenv("EDITOR", filepath.Join(tmp, "must-not-run"))
	}
	oldTerminal, oldConfirmEdit := isTerminal, confirmEdit
	calls := new(int)
	isTerminal = func() bool { return terminal }
	confirmEdit = func(q string) bool {
		*calls++
		if q != "Re-edit?" {
			t.Errorf("confirmEdit prompt = %q, want Re-edit?", q)
		}
		return answer
	}
	t.Cleanup(func() {
		isTerminal, confirmEdit = oldTerminal, oldConfirmEdit
		matches, err := filepath.Glob(filepath.Join(tmp, "claude-code-config-*"))
		if err != nil {
			t.Error(err)
		}
		if len(matches) != 0 {
			t.Errorf("Edit leaked temp files: %v", matches)
		}
	})
	return tmp, calls
}

func TestEdit(t *testing.T) {
	tests := []struct {
		mode, env, stdout string
		wantErr           bool
	}{
		{"pin", "VISUAL", "saved\n", false},
		{"pin", "EDITOR", "saved\n", false},
		{"noop", "VISUAL", "no changes\n", false},
		{"bad", "EDITOR", "", true},
		{"race", "VISUAL", "", true},
		{"fail", "EDITOR", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.mode+"/"+tt.env, func(t *testing.T) {
			tmp, calls := setupTestEditor(t, tt.mode, tt.env, false, false)
			before := exportTestDoc(t)
			var stdout, stderr bytes.Buffer
			err := Edit(&stdout, &stderr)
			if (err != nil) != tt.wantErr {
				t.Errorf("Edit() = %v, want error %v", err, tt.wantErr)
			}
			if stdout.String() != tt.stdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.stdout)
			}
			if stderr.Len() != 0 {
				t.Errorf("non-terminal Edit printed stderr: %q", stderr.String())
			}
			if *calls != 0 {
				t.Errorf("non-terminal Edit called confirmEdit %d times", *calls)
			}
			if seen, err := os.ReadFile(filepath.Join(tmp, "seen")); err != nil || string(seen) != "seen" {
				t.Fatalf("helper did not inspect the temp file: %q, %v", seen, err)
			}
			switch tt.mode {
			case "pin":
				if pins := vaultTestState(t)[vault.Pins]; !reflect.DeepEqual(pins, map[string]string{"HARNESS_PROVIDER": "openai"}) {
					t.Errorf("saved pins = %v", pins)
				}
			case "race":
				if !errors.Is(err, ErrChanged) {
					t.Errorf("Edit() = %v, want ErrChanged", err)
				}
				if pins := vaultTestState(t)[vault.Pins]; !reflect.DeepEqual(pins, map[string]string{"HARNESS_OPUS_MODEL": "concurrent-model"}) {
					t.Errorf("conflicting Edit wrote pins: %v", pins)
				}
			default:
				if after := exportTestDoc(t); !bytes.Equal(before, after) {
					t.Error("unsaved Edit changed config")
				}
			}
			if tt.mode == "fail" && (err == nil || !strings.Contains(err.Error(), "editor "+os.Args[0]) || !strings.Contains(err.Error(), "exit status 3")) {
				t.Errorf("editor failure did not name editor and exit status: %v", err)
			}
		})
	}
}

func TestEditReedit(t *testing.T) {
	tests := []struct {
		name, mode string
		answer     bool
		calls      int
		saved      bool
	}{
		{"accept", "retry", true, 1, true},
		{"decline", "bad", false, 1, false},
		{"conflict", "race", true, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, calls := setupTestEditor(t, tt.mode, "VISUAL", true, tt.answer)
			before := exportTestDoc(t)
			var stdout, stderr bytes.Buffer
			err := Edit(&stdout, &stderr)
			if *calls != tt.calls {
				t.Errorf("confirmEdit called %d times, want %d", *calls, tt.calls)
			}
			if tt.saved {
				if err != nil || stdout.String() != "saved\n" {
					t.Errorf("re-edit = %v, stdout %q", err, stdout.String())
				}
				if pins := vaultTestState(t)[vault.Pins]; !reflect.DeepEqual(pins, map[string]string{"HARNESS_PROVIDER": "openai"}) {
					t.Errorf("re-edit saved pins = %v", pins)
				}
			} else {
				if err == nil || stdout.Len() != 0 {
					t.Errorf("unsaved re-edit = %v, stdout %q", err, stdout.String())
				}
				if tt.mode == "race" {
					if !errors.Is(err, ErrChanged) {
						t.Errorf("re-edit = %v, want ErrChanged", err)
					}
					if pins := vaultTestState(t)[vault.Pins]; !reflect.DeepEqual(pins, map[string]string{"HARNESS_OPUS_MODEL": "concurrent-model"}) {
						t.Errorf("conflicting re-edit wrote pins: %v", pins)
					}
				} else if after := exportTestDoc(t); !bytes.Equal(after, before) {
					t.Error("declined re-edit changed config")
				}
			}
			if tt.calls > 0 && !strings.Contains(stderr.String(), "config:") {
				t.Errorf("missing validation diagnostic: %q", stderr.String())
			}
			if tt.calls == 0 && stderr.Len() != 0 {
				t.Errorf("conflict printed stderr: %q", stderr.String())
			}
		})
	}
}
