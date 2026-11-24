package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadScopeNewerSchema(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(ScopesDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	rec := fmt.Sprintf(`{"schema":99,"team":"t","id":"i","pid":%d,"port":7,"future":"x"}`, os.Getpid())
	if err := os.WriteFile(scopeFile("t", "i"), []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	sc, err := ReadScope("t", "i")
	if err != nil || sc.PID != os.Getpid() || sc.Port != 7 {
		t.Fatalf("ReadScope = %+v, %v; want the newer record", sc, err)
	}
}

func TestScopeCloseRemovesFileAndLock(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	sc := Scope{Team: "t", ID: "i", PID: os.Getpid(), Start: "x", Port: 1, Started: time.Now()}
	cleanup, err := WriteScope(sc)
	if err != nil {
		t.Fatalf("WriteScope: %v", err)
	}
	file := scopeFile("t", "i")
	if _, err := os.Stat(file + ".lock"); err != nil {
		t.Fatalf("open scope has no lock file: %v", err)
	}
	cleanup()
	for _, p := range []string{file, file + ".lock"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("after close, %s still exists (stat err %v)", filepath.Base(p), err)
		}
	}
}
