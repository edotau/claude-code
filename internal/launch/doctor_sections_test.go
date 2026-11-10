package launch

import (
	"sync"
	"testing"
	"time"
)

// Given sections that each wait for every other to start, Then DoctorChecks runs them at once and keeps their order.
func TestDoctorChecksRunsSectionsConcurrentlyInOrder(t *testing.T) {
	orig := doctorSections
	t.Cleanup(func() { doctorSections = orig })
	var started sync.WaitGroup
	section := func(name string, hardFail bool, delay time.Duration) doctorSection {
		return func() ([]Check, bool) {
			started.Done()
			started.Wait() // a serial runner never gets past the first section
			time.Sleep(delay)
			return []Check{{Name: name + " 1"}, {Name: name + " 2"}}, !hardFail
		}
	}
	doctorSections = []doctorSection{section("a", false, 30*time.Millisecond), section("b", true, 0),
		section("c", false, 10*time.Millisecond), section("d", false, 0)}
	started.Add(len(doctorSections))
	done := make(chan struct{})
	var checks []Check
	var ok bool
	go func() { checks, ok = DoctorChecks(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sections ran one at a time")
	}
	var got []string
	for _, c := range checks {
		got = append(got, c.Name)
	}
	want := []string{"a 1", "a 2", "b 1", "b 2", "c 1", "c 2", "d 1", "d 2"}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rows = %v, want %v (section order)", got, want)
		}
	}
	if ok {
		t.Error("ok = true, want false: section b failed a hard check")
	}
}
