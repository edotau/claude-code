package proxyteam

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/assets"
)

// good is a valid three-team config: shared (default), a phase team and a standing team.
func good() Config {
	return Config{Schema: 1, Teams: []Team{
		{Name: "shared", Kinds: []Kind{KindInteractive}, Host: HostDaemon, Lifetime: LifetimeShared, Reserved: 1, Borrow: BorrowAny, Default: true},
		{Name: "general", Host: HostDetached, Lifetime: LifetimeStanding, Reserved: 1, Borrow: BorrowAny, Fallback: []string{"shared"}, Kinds: []Kind{KindSingle}},
		{Name: "phase", Kinds: []Kind{KindGroup}, Host: HostPhase, Lifetime: LifetimePhase, Reserved: 1, Borrow: BorrowNever, Fallback: []string{"general", "shared"}},
	}}
}

func TestEmbeddedDefaultValidates(t *testing.T) {
	c, ps := Parse(assets.ProxyTeams(), nil)
	if len(ps) != 0 {
		t.Fatalf("embedded default has problems: %v", ps)
	}
	if len(c.Teams) == 0 {
		t.Fatal("embedded default has no teams")
	}
}

var problemCases = []struct {
	name   string
	mutate func(*Config)
	field  string
	detail string
}{
	{"clean", func(*Config) {}, "", ""},
	{"unknown kind", func(c *Config) { c.Teams[2].Kinds = []Kind{"nope"} }, "kinds", "unknown kind"},
	{"unknown host", func(c *Config) { c.Teams[2].Host = "nope" }, "host", "unknown host"},
	{"unknown lifetime", func(c *Config) { c.Teams[2].Lifetime = "nope" }, "lifetime", "unknown lifetime"},
	{"unknown borrow", func(c *Config) { c.Teams[2].Borrow = "nope" }, "borrow", "unknown borrow"},
	{"empty name", func(c *Config) { c.Teams[2].Name = "" }, "name", "empty name"},
	{"repeated name", func(c *Config) { c.Teams[2].Name = "general" }, "name", "more than once"},
	{"kind claimed twice", func(c *Config) { c.Teams[2].Kinds = []Kind{KindSingle} }, "kinds", "already claimed"},
	{"no default", func(c *Config) { c.Teams[0].Default = false }, "default", "have 0"},
	{"two defaults", func(c *Config) { c.Teams[2].Default = true }, "default", "have 2"},
	{"no shared", func(c *Config) { c.Teams[0].Host, c.Teams[0].Lifetime = HostAsk, LifetimeAsk }, "lifetime", "have 0"},
	{"two shared", func(c *Config) { c.Teams[1].Host, c.Teams[1].Lifetime = HostDaemon, LifetimeShared }, "lifetime", "have 2"},
	{"negative reserved", func(c *Config) { c.Teams[2].Reserved = -1 }, "reserved", "negative"},
	{"zero reserved on shared", func(c *Config) { c.Teams[0].Reserved = 0 }, "reserved", "shared team"},
	{"host and lifetime unpaired", func(c *Config) { c.Teams[2].Lifetime = LifetimeAsk }, "lifetime", "runs with"},
	{"empty fallback", func(c *Config) { c.Teams[2].Fallback = nil }, "fallback", "empty"},
	{"unknown fallback", func(c *Config) { c.Teams[2].Fallback = []string{"ghost", "shared"} }, "fallback", "unknown team"},
	{"fallback to itself", func(c *Config) { c.Teams[2].Fallback = []string{"phase", "shared"} }, "fallback", "itself"},
	{"fallback cycle", func(c *Config) { c.Teams[1].Fallback = []string{"phase", "shared"} }, "fallback", "loops back"},
	{"chain misses shared", func(c *Config) { c.Teams[2].Fallback = []string{"general"} }, "fallback", "not the shared team"},
	{"shared team falls back", func(c *Config) { c.Teams[0].Fallback = []string{"general"} }, "fallback", "no fallback"},
	{"shared lacks interactive", func(c *Config) { c.Teams[0].Kinds = []Kind{KindLane} }, "kinds", "must claim"},
	{"kind listed twice", func(c *Config) { c.Teams[2].Kinds = []Kind{KindGroup, KindGroup} }, "kinds", "more than once"},
	{"fallback never lends", func(c *Config) { c.Teams[1].Fallback = []string{"phase", "shared"} }, "fallback", "never lends"},
	{"wrong schema", func(c *Config) { c.Schema = 0 }, "schema", "want 1"},
	{"empty kinds off default", func(c *Config) { c.Teams[2].Kinds = nil }, "kinds", "not the default"},
}

func TestValidateProblems(t *testing.T) {
	for _, tc := range problemCases {
		t.Run(tc.name, func(t *testing.T) {
			c := good()
			tc.mutate(&c)
			ps := Validate(c)
			if tc.field == "" {
				if len(ps) != 0 {
					t.Fatalf("clean config has problems: %v", ps)
				}
				return
			}
			for _, p := range ps {
				if p.Field == tc.field && strings.Contains(p.Detail, tc.detail) {
					return
				}
			}
			t.Fatalf("no %s problem containing %q in %v", tc.field, tc.detail, ps)
		})
	}
}

func TestEveryKindResolves(t *testing.T) {
	d, _ := Parse(assets.ProxyTeams(), nil)
	for _, c := range []Config{d, good(), {}} {
		kinds := append(Kinds(), "unknown-kind", "")
		for i := range 1 << 6 {
			kinds = append(kinds, KindOf(Facts{i&1 > 0, i&2 > 0, i&4 > 0, i&8 > 0, i&16 > 0, i&32 > 0}))
		}
		for _, k := range kinds {
			routes := c.Routes(k)
			if len(routes) == 0 || routes[0].Name != c.TeamFor(k).Name {
				t.Fatalf("Routes(%q) = %v does not start at TeamFor", k, routes)
			}
			if len(c.Teams) > 0 && routes[len(routes)-1].Lifetime != LifetimeShared {
				t.Fatalf("Routes(%q) = %v does not end at the shared team", k, routes)
			}
		}
	}
}

func TestRoutesTerminateOnBrokenConfig(t *testing.T) {
	c := Config{Teams: []Team{
		{Name: "a", Fallback: []string{"b", "a", "ghost"}, Default: true},
		{Name: "b", Fallback: []string{"a"}},
	}}
	if got := c.Routes(KindLane); len(got) != 2 {
		t.Fatalf("Routes on a cyclic config = %v, want the two teams once each", got)
	}
}

func TestFieldsCoverEveryTeamField(t *testing.T) {
	if got, want := len(Team{}.Fields()), reflect.TypeFor[Team]().NumField(); got != want {
		t.Fatalf("Fields lists %d values, Team has %d fields", got, want)
	}
	names := map[string]bool{}
	for _, f := range good().Teams[2].Fields() {
		names[f.Name] = true
	}
	for f := range reflect.TypeFor[Team]().Fields() {
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !names[tag] {
			t.Errorf("Fields has no entry for json field %q", tag)
		}
	}
}

func TestOverlayReplacesOrAdds(t *testing.T) {
	overlay := `{"schema":1,"teams":[` +
		`{"name":"shared","kinds":["interactive","lane"],"host":"daemon","lifetime":"shared","reserved":2,"borrow":"any","fallback":[],"default":true},` +
		`{"name":"phase","kinds":["group"],"host":"phase","lifetime":"phase","reserved":1,"borrow":"never","fallback":["shared"]}]}`
	c, ps := Parse(assets.ProxyTeams(), []byte(overlay))
	if len(ps) != 0 || len(c.Teams) != 2 || c.Teams[0].Reserved != 2 || c.TeamFor(KindLane).Name != "shared" {
		t.Fatalf("merge = %+v, problems %v", c, ps)
	}
	dup := `{"schema":1,"teams":[{"name":"x","kinds":["lane"],"host":"ask","lifetime":"ask","reserved":1,"borrow":"never","fallback":["shared"]},` +
		`{"name":"x","kinds":["nested"],"host":"ask","lifetime":"ask","reserved":1,"borrow":"never","fallback":["shared"]}]}`
	if _, ps := Parse(assets.ProxyTeams(), []byte(dup)); len(ps) == 0 {
		t.Fatal("two overlay teams of one name validated")
	}
}

func TestBadOverlaysAreProblems(t *testing.T) {
	for name, overlay := range map[string]string{
		"not json":      `{`,
		"newer schema":  `{"schema":9,"teams":[]}`,
		"no schema":     `{"teams":[]}`,
		"unknown field": `{"schema":1,"teams":[{"name":"x","fallbak":["shared"]}]}`,
	} {
		if _, ps := Parse(assets.ProxyTeams(), []byte(overlay)); len(ps) == 0 {
			t.Errorf("%s: no problem", name)
		}
	}
}

func TestBadOverlayKeepsLastGood(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	goodOverlay := `{"schema":1,"teams":[{"name":"phase","kinds":["group"],"host":"phase","lifetime":"phase","reserved":1,"borrow":"never","fallback":["shared"]}]}`
	bad := `{"schema":1,"teams":[{"name":"phase","kinds":["group"],"host":"phase","lifetime":"phase","reserved":1,"borrow":"never","fallback":[]}]}`

	def, _ := Parse(assets.ProxyTeams(), nil)
	if c, ps := Load([]byte(bad)); len(ps) == 0 || !reflect.DeepEqual(c, def) {
		t.Fatalf("no last-good: got %+v %v, want the embedded default and problems", c, ps)
	}
	want, ps := Load([]byte(goodOverlay))
	if len(ps) != 0 || len(want.Teams) != 2 {
		t.Fatalf("good overlay: %+v %v", want, ps)
	}
	if c, ps := Load([]byte(bad)); len(ps) == 0 || !reflect.DeepEqual(c, want) {
		t.Fatalf("bad overlay: got %+v %v, want the last good config and problems", c, ps)
	}
	if err := os.WriteFile(lastGoodPath(), []byte(`{"schema":1,"teams":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _ := Load([]byte(bad)); !reflect.DeepEqual(c, def) {
		t.Fatalf("corrupt last-good: got %+v, want the embedded default", c)
	}
}

func TestCleanLoadRefreshesLastGood(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	overlay := `{"schema":1,"teams":[{"name":"phase","kinds":["group"],"host":"phase","lifetime":"phase","reserved":1,"borrow":"never","fallback":["shared"]}]}`
	Load([]byte(overlay))
	def, _ := Load(nil)
	if c, ps := Load([]byte(`{`)); len(ps) == 0 || !reflect.DeepEqual(c, def) {
		t.Fatalf("a removed overlay came back: %+v %v", c, ps)
	}
}

func TestRoutesAreCopies(t *testing.T) {
	c := good()
	c.TeamFor(KindGroup).Fallback[0] = "x"
	c.Routes(KindGroup)[0].Kinds[0] = "x"
	if ps := Validate(c); len(ps) != 0 {
		t.Fatalf("a returned team aliases the config: %v", ps)
	}
}

func TestOverlayProblemNamesItsSource(t *testing.T) {
	overlay := `{"schema":1,"teams":[{"name":"general","kinds":["group"],"host":"ask","lifetime":"ask","reserved":1,` +
		`"borrow":"never","fallback":["shared"]}]}`
	_, ps := Parse(assets.ProxyTeams(), []byte(overlay))
	if len(ps) == 0 || !strings.Contains(ps[0].String(), "vault overlay proxy-teams; remove: claude-code vault overlay unset proxy-teams --yes") {
		t.Fatalf("problems %v: want the overlay source and the remove command", ps)
	}
	if _, ps := Parse(assets.ProxyTeams(), nil); len(ps) != 0 {
		t.Fatalf("default config has problems %v", ps)
	}
}
