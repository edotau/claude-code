package proc

import (
	"context"
	"maps"
	"reflect"
	"sync"
	"testing"
)

func TestValuesShapes(t *testing.T) {
	for _, tt := range []struct {
		name      string
		in        Values
		env, args []string
		json      string
	}{
		{"nil", nil, []string{}, []string{}, "{}"},
		{"empty", Values{}, []string{}, []string{}, "{}"},
		{"values", Values{"Z": "x=y", "A": ""}, []string{"A=", "Z=x=y"},
			[]string{"--set", "A=", "--set", "Z=x=y"}, `{"A":"","Z":"x=y"}`},
		{"escaping", Values{"Q": "\"\n"}, []string{"Q=\"\n"}, []string{"--set", "Q=\"\n"}, `{"Q":"\"\n"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.Env(); !reflect.DeepEqual(got, tt.env) {
				t.Errorf("Env() = %q, want %q", got, tt.env)
			}
			if got := tt.in.Args("--set"); !reflect.DeepEqual(got, tt.args) {
				t.Errorf("Args() = %q, want %q", got, tt.args)
			}
			got, err := tt.in.JSON()
			if err != nil || string(got) != tt.json {
				t.Errorf("JSON() = %s, %v; want %s", got, err, tt.json)
			}
		})
	}
}

func TestValuesFromEnv(t *testing.T) {
	for _, tt := range []struct {
		name string
		env  []string
		want Values
	}{
		{"nil", nil, Values{}},
		{"invalid", []string{"missing", "=unnamed"}, Values{}},
		{"empty", []string{"A="}, Values{"A": ""}},
		{"equals", []string{"A=x=y"}, Values{"A": "x=y"}},
		{"duplicate", []string{"A=old", "A=new"}, Values{"A": "new"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValuesFromEnv(tt.env); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ValuesFromEnv(%q) = %v, want %v", tt.env, got, tt.want)
			}
		})
	}
}

func TestValuesRoundTrip(t *testing.T) {
	for _, in := range []Values{nil, {}, {"Z": "x=y", "A": ""}} {
		if got := ValuesFromEnv(in.Env()); !maps.Equal(got, in) {
			t.Errorf("round trip = %v, want %v", got, in)
		}
	}
}

func TestValuesCopies(t *testing.T) {
	for _, in := range []Values{nil, {}, {"A": "old", "B": "keep"}} {
		for _, op := range []string{"with", "merge"} {
			t.Run(op, func(t *testing.T) {
				before := maps.Clone(in)
				over := Values{"A": "new"}
				var got Values
				if op == "with" {
					got = in.With("A", "new")
				} else {
					got = in.Merge(over)
				}
				want := maps.Clone(before)
				if want == nil {
					want = Values{}
				}
				want["A"] = "new"
				if !maps.Equal(got, want) {
					t.Errorf("copy = %v, want %v", got, want)
				}
				got["B"] = "changed"
				if !reflect.DeepEqual(in, before) || over["A"] != "new" {
					t.Errorf("inputs mutated: receiver %v, overlay %v", in, over)
				}
			})
		}
	}
}

func TestValueSnapshotContract(t *testing.T) {
	t.Setenv("PROC_KEYED_X", "ambient")
	bg := context.Background()
	if got := Value(bg, "PROC_KEYED_X"); got != "ambient" {
		t.Errorf("no snapshot: Value = %q, want the process environment", got)
	}
	in := Values{"PROC_KEYED_X": "run", "PROC_KEYED_EMPTY": ""}
	ctx := WithValues(bg, in)
	if got := Value(ctx, "PROC_KEYED_X"); got != "run" {
		t.Errorf("Value = %q, want the snapshot value over the environment", got)
	}
	t.Setenv("PROC_KEYED_EMPTY", "ambient")
	t.Setenv("PROC_KEYED_MISSING", "ambient")
	for _, k := range []string{"PROC_KEYED_EMPTY", "PROC_KEYED_MISSING"} {
		if got := Value(ctx, k); got != "" {
			t.Errorf("Value(%s) = %q, want the snapshot to suppress the environment", k, got)
		}
	}
	in["PROC_KEYED_X"] = "mutated"
	got, ok := ValuesFromContext(ctx)
	if !ok || got["PROC_KEYED_X"] != "run" {
		t.Fatalf("mutating the input changed the snapshot: %v, %v", got, ok)
	}
	got["PROC_KEYED_X"] = "mutated"
	if v := Value(ctx, "PROC_KEYED_X"); v != "run" {
		t.Errorf("mutating the returned copy changed the snapshot: %q", v)
	}
}

func TestValueConcurrentContexts(t *testing.T) {
	a := WithValues(context.Background(), Values{"K": "a"})
	b := WithValues(context.Background(), Values{"K": "b"})
	var wg sync.WaitGroup
	for range 50 {
		for ctx, want := range map[context.Context]string{a: "a", b: "b"} {
			wg.Go(func() {
				if got := Value(ctx, "K"); got != want {
					t.Errorf("Value = %q, want %q", got, want)
				}
			})
		}
	}
	wg.Wait()
}
