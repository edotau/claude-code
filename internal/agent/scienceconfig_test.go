package agent

import (
	"slices"
	"testing"
)

func TestScienceConfigPortTopLevelOnly(t *testing.T) {
	cases := map[string]int{
		"port = 4100\n":                  4100,
		"port = 4100 # app\n[sandbox]\n": 4100,
		"[sandbox]\nport = 4100\n":       3000,
		"":                               3000,
		"port = nope\n":                  3000,
	}
	for in, want := range cases {
		if got := scienceConfigPort([]byte(in)); got != want {
			t.Errorf("scienceConfigPort(%q) = %d, want %d", in, got, want)
		}
	}
	if got := scienceConfigPort(scienceTOML); got != 3000 {
		t.Errorf("embedded science.toml port = %d, want 3000", got)
	}
}

func TestScienceArgsLeadingPortOnly(t *testing.T) {
	port, rest, err := scienceArgs([]string{"--port=4100", "serve", "--port", "9"}, 3000)
	if err != nil || port != 4100 || !slices.Equal(rest, []string{"serve", "--port", "9"}) {
		t.Fatalf("got %d %v %v", port, rest, err)
	}
	if _, _, err := scienceArgs([]string{"--port", "x"}, 3000); err == nil {
		t.Fatal("bad --port must error")
	}
}

func TestScienceArgvPinsServeAndConfig(t *testing.T) {
	free := func(int) bool { return false }
	got, err := scienceArgv([]string{"serve"}, "/c.toml", 3000, free)
	want := []string{"serve", "--config", "/c.toml", "--port", "3000", "--sandbox-port", "3001"}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("serve argv = %v, %v; want %v", got, err, want)
	}
	if got, _ := scienceArgv([]string{"status", "--config", "x"}, "/c.toml", 3000, free); !slices.Equal(got, []string{"status", "--config", "x"}) {
		t.Errorf("explicit --config must win: %v", got)
	}
	if got, _ := scienceArgv([]string{"serve", "--port", "5"}, "", 3000, free); !slices.Equal(got, []string{"serve", "--port", "5"}) {
		t.Errorf("vendor's own serve --port must pass untouched: %v", got)
	}
	busy := func(p int) bool { return p == 3001 }
	if _, err := scienceArgv([]string{"serve"}, "", 3000, busy); err == nil {
		t.Error("a busy sandbox port must error, not drift")
	}
}
