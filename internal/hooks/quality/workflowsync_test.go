package quality

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkflowSyncRepair(t *testing.T) {
	root, canonical := writeSyncFixture(t)
	dir := filepath.Join(root, "workflows")
	path := filepath.Join(dir, "canonical.js")
	if rc, _, errs := runLint(root); rc != 1 || !strings.Contains(errs, "differs") {
		t.Fatalf("before: rc=%d stderr=%s", rc, errs)
	}
	rc, out, errs := runSync(path)
	if rc != 0 || strings.Count(out, "\n") != 3 || !strings.Contains(out, "synced "+filepath.Join(dir, "target.js")) {
		t.Fatalf("sync: rc=%d stdout=%s stderr=%s", rc, out, errs)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != canonical {
		t.Fatalf("canonical changed: %q %v", data, err)
	}
	assertSyncedClean(t, root, path)
}

func assertSyncedClean(t *testing.T, root, path string) {
	t.Helper()
	if rc, _, errs := runLint(root); rc != 0 {
		t.Fatalf("after: rc=%d stderr=%s", rc, errs)
	}
	if rc, out, errs := runSync(path); rc != 0 || strings.Count(out, "unchanged ") != 3 {
		t.Fatalf("idempotence: rc=%d stdout=%s stderr=%s", rc, out, errs)
	}
}

func writeSyncFixture(t *testing.T) (root, canonical string) {
	t.Helper()
	root = writeDriftRoot(t, nil, nil)
	dir := filepath.Join(root, "workflows")
	step := helperStart + " shared\nfunction stepOf() {}\n" + helperEnd + "\n"
	stages := stagesStart + " shared\nfunction stageLoop() {}\n" + stagesEnd + "\n"
	canonical = okBody + step + stages
	writeScript(t, dir, "canonical.js", canonical)
	writeScript(t, dir, "target.js", strings.Replace(canonical, "shared", "changed", 1))
	writeScript(t, dir, "step-only.js", okBody+step)
	writeScript(t, dir, "none.js", okBody)
	return root, canonical
}

func runLint(root string) (int, string, string) {
	var out, errb bytes.Buffer
	rc := RunWorkflowLint([]string{root}, &out, &errb)
	return rc, out.String(), errb.String()
}

func runSync(path string) (int, string, string) {
	var out, errb bytes.Buffer
	rc := RunWorkflowSync([]string{path}, &out, &errb)
	return rc, out.String(), errb.String()
}

func TestWorkflowSyncMissingMarkers(t *testing.T) {
	for _, body := range []string{"stepOf()\n", helperStart + " broken\n", helperEnd + "\n"} {
		t.Run(body, func(t *testing.T) {
			root := writeDriftRoot(t, nil, nil)
			dir := filepath.Join(root, "workflows")
			canonical := helperStart + " shared\nfunction stepOf() {}\n" + helperEnd + "\n"
			writeScript(t, dir, "canonical.js", canonical)
			writeScript(t, dir, "broken.js", body)
			var out, errb bytes.Buffer
			if rc := RunWorkflowSync([]string{filepath.Join(dir, "canonical.js")}, &out, &errb); rc != 1 || !strings.Contains(errb.String(), "no stepOf block") {
				t.Fatalf("rc=%d stdout=%s stderr=%s", rc, &out, &errb)
			}
			data, err := os.ReadFile(filepath.Join(dir, "broken.js"))
			if err != nil || string(data) != body {
				t.Fatalf("broken target changed: %q %v", data, err)
			}
		})
	}
}

func TestWorkflowSyncUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"a.js", "b.js"}, {"--bad"}, {"not-js"}} {
		var out, errb bytes.Buffer
		if rc := RunWorkflowSync(args, &out, &errb); rc != 2 {
			t.Fatalf("args=%v rc=%d", args, rc)
		}
	}
	var out, errb bytes.Buffer
	if rc := RunWorkflowSync([]string{"-h"}, &out, &errb); rc != 0 || !strings.Contains(out.String(), "2 usage") {
		t.Fatalf("help: rc=%d out=%s", rc, &out)
	}
}
