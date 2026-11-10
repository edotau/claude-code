package paths

import (
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMemoClock pins memoNow to a settable instant for the test.
func fakeMemoClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Unix(1_000_000, 0)
	orig := memoNow
	memoNow = func() time.Time { return now }
	t.Cleanup(func() { memoNow = orig })
	return &now
}

func keepFor(d time.Duration) func(int, error) time.Duration {
	return func(int, error) time.Duration { return d }
}

func TestMemoKeep(t *testing.T) {
	for _, c := range []struct {
		name    string
		keep    time.Duration
		advance time.Duration
		want    int32 // fn runs after the second Do
	}{
		{"zero keeps forever", 0, 24 * time.Hour, 1},
		{"negative never keeps", -1, 0, 2},
		{"positive within window", 10 * time.Second, 9 * time.Second, 1},
		{"positive expired", 10 * time.Second, 10 * time.Second, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			now := fakeMemoClock(t)
			var m Memo[string, int]
			var runs atomic.Int32
			fn := func() (int, error) { return int(runs.Add(1)), nil }
			if v, _ := m.Do("k", keepFor(c.keep), fn); v != 1 {
				t.Fatalf("first Do = %d; want 1", v)
			}
			*now = now.Add(c.advance)
			if v, _ := m.Do("k", keepFor(c.keep), fn); v != int(c.want) || runs.Load() != c.want {
				t.Errorf("second Do = %d after %d runs; want %d", v, runs.Load(), c.want)
			}
		})
	}
}

func TestMemoDrop(t *testing.T) {
	var m Memo[string, int]
	var runs atomic.Int32
	fn := func() (int, error) { return int(runs.Add(1)), nil }
	m.Do("a", keepFor(0), fn)
	m.Drop()
	if v, _ := m.Do("a", keepFor(0), fn); v != 2 {
		t.Errorf("Do after Drop = %d; want 2 (fn reruns)", v)
	}
}

func TestMemoErrorHonoursKeep(t *testing.T) {
	fakeMemoClock(t)
	boom := errors.New("boom")
	var m Memo[string, int]
	var runs atomic.Int32
	fn := func() (int, error) { runs.Add(1); return 0, boom }
	keep := func(_ int, err error) time.Duration {
		if err != nil {
			return 5 * time.Second
		}
		return -1
	}
	m.Do("k", keep, fn)
	if _, err := m.Do("k", keep, fn); !errors.Is(err, boom) || runs.Load() != 1 {
		t.Errorf("kept error = %v after %d runs; want boom after 1", err, runs.Load())
	}
}

// holdOpen starts Do on key with an fn that answers want only after release closes; returns once fn runs.
func holdOpen(m *Memo[string, int], key string, want int) (release chan struct{}, got chan int) {
	started := make(chan struct{})
	release, got = make(chan struct{}), make(chan int, 1)
	go func() {
		v, _ := m.Do(key, keepFor(0), func() (int, error) { close(started); <-release; return want, nil })
		got <- v
	}()
	<-started
	return release, got
}

func TestMemoSingleFlightPerKey(t *testing.T) {
	var m Memo[string, int]
	release, gotA := holdOpen(&m, "k", 1)
	var runsB atomic.Int32
	gotB := make(chan int, 1)
	go func() {
		v, _ := m.Do("k", keepFor(0), func() (int, error) { runsB.Add(1); return 2, nil })
		gotB <- v
	}()
	select {
	case v := <-gotB:
		t.Fatalf("second caller returned %d while the first run was open; want it to wait", v)
	case <-time.After(50 * time.Millisecond): // gives B time to join; a passing run never depends on it
	}
	close(release)
	if a, b := <-gotA, <-gotB; a != 1 || b != 1 || runsB.Load() != 0 {
		t.Errorf("answers %d, %d with second fn run %d times; want 1, 1, 0", a, b, runsB.Load())
	}
}

func TestMemoKeysDoNotBlockEachOther(t *testing.T) {
	var m Memo[string, int]
	release, gotA := holdOpen(&m, "a", 1)
	defer func() { close(release); <-gotA }()
	done := make(chan int, 1)
	go func() { v, _ := m.Do("b", keepFor(0), func() (int, error) { return 2, nil }); done <- v }()
	select {
	case v := <-done:
		if v != 2 {
			t.Errorf("key b = %d; want 2", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("key b waited on key a's open run")
	}
}

// memoLen is m's entry count, read under its lock.
func memoLen[K comparable, V any](m *Memo[K, V]) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.m)
}

func TestMemoSweepsExpiredKeys(t *testing.T) {
	now := fakeMemoClock(t)
	var m Memo[int, int]
	var runs atomic.Int32
	fn := func() (int, error) { return int(runs.Add(1)), nil }
	m.Do(-1, keepFor(0), fn) // keep-forever: never swept
	peak := 0
	for i := range 1000 {
		m.Do(i, keepFor(time.Second), fn)
		*now = now.Add(2 * time.Second)
		peak = max(peak, memoLen(&m))
	}
	if limit := 2 + memoSweepMin; peak > limit {
		t.Errorf("map peaked at %d entries after 1000 expired keys; want at most %d", peak, limit)
	}
	if v, _ := m.Do(-1, keepFor(0), fn); v != 1 {
		t.Errorf("keep-forever entry = %d; want 1 (swept and rerun)", v)
	}
}

func TestMemoSweepKeepsRunInFlight(t *testing.T) {
	now := fakeMemoClock(t)
	var m Memo[string, int]
	release, got := holdOpen(&m, "open", 1)
	for i := range 100 {
		m.Do(strconv.Itoa(i), keepFor(time.Second), func() (int, error) { return 0, nil })
		*now = now.Add(2 * time.Second)
	}
	var reruns atomic.Int32
	joined := make(chan int, 1)
	go func() {
		v, _ := m.Do("open", keepFor(0), func() (int, error) { reruns.Add(1); return 2, nil })
		joined <- v
	}()
	close(release)
	if a, b := <-got, <-joined; a != 1 || b != 1 || reruns.Load() != 0 {
		t.Errorf("answers %d, %d with rerun %d; want 1, 1, 0 (the open run was swept)", a, b, reruns.Load())
	}
}

func TestMemoCapsSettledEntries(t *testing.T) {
	fakeMemoClock(t)
	var m Memo[int, int]
	var runs atomic.Int32
	fn := func() (int, error) { return int(runs.Add(1)), nil }
	peak := 0
	for i := range 1000 {
		m.Do(i, keepFor(0), fn)
		peak = max(peak, memoLen(&m))
	}
	t.Logf("peak map size after 1000 keep-forever keys: %d", peak)
	if peak > memoCap {
		t.Errorf("map peaked at %d entries after 1000 keep-forever keys; want at most %d", peak, memoCap)
	}
}

func TestMemoEvictsOldestKeepsNewest(t *testing.T) {
	fakeMemoClock(t)
	var m Memo[int, int]
	var runs atomic.Int32
	fn := func() (int, error) { return int(runs.Add(1)), nil }
	for i := range memoCap + 10 {
		m.Do(i, keepFor(0), fn)
	}
	before := runs.Load()
	if v, _ := m.Do(memoCap+9, keepFor(0), fn); v != int(before) || runs.Load() != before {
		t.Errorf("newest key = %d with %d runs; want kept (%d runs)", v, runs.Load(), before)
	}
	if v, _ := m.Do(0, keepFor(0), fn); v != int(before)+1 || runs.Load() != before+1 {
		t.Errorf("oldest key = %d with %d runs; want evicted and rerun (value %d)", v, runs.Load(), before+1)
	}
}

func TestMemoCapNeverEvictsRunInFlight(t *testing.T) {
	fakeMemoClock(t)
	var m Memo[string, int]
	release, got := holdOpen(&m, "open", 1)
	for i := range 3 * memoCap {
		m.Do(strconv.Itoa(i), keepFor(0), func() (int, error) { return 0, nil })
	}
	var reruns atomic.Int32
	joined := make(chan int, 1)
	go func() {
		v, _ := m.Do("open", keepFor(0), func() (int, error) { reruns.Add(1); return 2, nil })
		joined <- v
	}()
	close(release)
	if a, b := <-got, <-joined; a != 1 || b != 1 || reruns.Load() != 0 {
		t.Errorf("answers %d, %d with rerun %d; want 1, 1, 0 (the open run was evicted)", a, b, reruns.Load())
	}
}
