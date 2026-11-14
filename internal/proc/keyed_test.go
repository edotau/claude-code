package proc

import (
	"maps"
	"reflect"
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
