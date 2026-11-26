package memory

import (
	"strings"
	"testing"
)

func TestBankStoreLoadReturnsSixDocs(t *testing.T) {
	batchStore(t)
	docs, _, err := newBankStore(false).Load()
	if err != nil || len(docs) != len(bankFiles) {
		t.Fatalf("docs=%d err=%v", len(docs), err)
	}
}

func TestBankStoreApplyWritesAdd(t *testing.T) {
	batchStore(t)
	rs, err := newBankStore(false).Apply(Batch{Ops: []Op{batchAdd}}, nil, testEnv().Now)
	if err != nil || len(rs) != 1 || rs[0].Outcome != DreamOK {
		t.Fatalf("rs=%+v err=%v", rs, err)
	}
	if got := mustRead(t, Path("decisionLog.md")); !strings.Contains(got, " — n <!-- id:") {
		t.Errorf("not written:\n%s", got)
	}
}

func TestBankStoreApplyLocked(t *testing.T) {
	batchStore(t)
	err := withFlatLock(BankLockWait, func() error {
		_, err := newBankStore(true).Apply(Batch{Ops: []Op{batchAdd}}, nil, testEnv().Now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBankStoreLoadVersions(t *testing.T) {
	batchStore(t)
	write(t, Path("progress.md"), "# Progress\n\n## In Progress\n\n- Dup\n- Dup\n- Solo\n")
	_, v, err := newBankStore(false).Load()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := v[contentRef("- Dup")]; !ok || got != "" {
		t.Errorf("duplicate ref version = %q, %v; want empty", got, ok)
	}
	if v[contentRef("- Solo")] == "" {
		t.Error("unique ref has no version")
	}
}
