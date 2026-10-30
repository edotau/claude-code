package configure

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/tui"
	"github.com/edotau/claude-code/internal/vendors"
)

// ocFake stubs every opencode page seam: no real opencode, terminal or keyring is touched.
type ocFake struct {
	ints        []vendors.OpencodeIntegration
	err         error
	major       int
	answers     []tui.ListResult // popped per List call; exhausted = Esc
	specs       []tui.ListSpec
	ran         [][]string
	cancel      bool
	runErr      error
	invalidated int
	confirm     bool
	asked       []string
}

func installOCFake(t *testing.T, f *ocFake) {
	t.Helper()
	oa, om, ol, osw, oi, oli, ora, oc, olk := opencodeAuth, opencodeMajor, opencodeLogoutArgv, opencodeSwitchArgv,
		invalidateOpencodeAuth, opencodeList, runAttached, confirmKey, lookOpencode
	t.Cleanup(func() {
		opencodeAuth, opencodeMajor, opencodeLogoutArgv, opencodeSwitchArgv = oa, om, ol, osw
		invalidateOpencodeAuth, opencodeList, runAttached, confirmKey, lookOpencode = oi, oli, ora, oc, olk
	})
	opencodeAuth = func() ([]vendors.OpencodeIntegration, error) { return f.ints, f.err }
	opencodeMajor = func() int { return f.major }
	opencodeLogoutArgv = func(id, cred string) ([]string, error) {
		if f.major >= 2 {
			return []string{"/real/opencode", "auth", "logout", id, cred}, nil
		}
		return []string{"/real/opencode", "auth", "logout", id}, nil
	}
	opencodeSwitchArgv = func(id, cred string) ([]string, error) {
		if f.major >= 2 {
			return []string{"/real/opencode", "auth", "switch", id, cred}, nil
		}
		return nil, nil
	}
	invalidateOpencodeAuth = func() { f.invalidated++ }
	opencodeList = func(spec tui.ListSpec, start int) (tui.ListResult, error) {
		f.specs = append(f.specs, spec)
		if len(f.answers) == 0 {
			return tui.ListResult{Index: -1}, nil
		}
		a := f.answers[0]
		f.answers = f.answers[1:]
		return a, nil
	}
	runAttached = func(argv []string) (bool, error) {
		f.ran = append(f.ran, argv)
		return f.cancel, f.runErr
	}
	confirmKey = func(q string, def bool) bool { f.asked = append(f.asked, q); return f.confirm }
	lookOpencode = func(string) (string, error) { return "/real/opencode", nil }
}

func ocDialog() (*dialog, *bytes.Buffer) {
	var out bytes.Buffer
	return &dialog{out: &out}, &out
}

// v1Data is the 1.x shape: one credential per integration, Name == ID, labels OAuth / API key.
func v1Data() []vendors.OpencodeIntegration {
	cred := func(id, label, method string) vendors.OpencodeIntegration {
		return vendors.OpencodeIntegration{ID: id, Name: id, Conns: []vendors.OpencodeConn{{Kind: "credential", ID: id, Label: label, Method: method}}}
	}
	return []vendors.OpencodeIntegration{
		cred("databricks", "API key", "key"), cred("github-copilot", "OAuth", "oauth"),
		cred("nvidia", "API key", "key"), cred("openai", "OAuth", "oauth"), cred("zed", "API key", "key"),
	}
}

// v2Data is the 2.x shape: an active credential plus an env connection, and one needing sign-in.
func v2Data() []vendors.OpencodeIntegration {
	return []vendors.OpencodeIntegration{
		{ID: "github-copilot", Name: "GitHub Copilot", Conns: []vendors.OpencodeConn{
			{Kind: "credential", ID: "cred-1", Label: "work", Method: "oauth", Active: true},
			{Kind: "credential", ID: "cred-2", Label: "personal", Method: "oauth"},
			{Kind: "env", ID: "GITHUB_TOKEN", Label: "GITHUB_TOKEN"},
		}},
		{ID: "openai", Name: "OpenAI", Conns: []vendors.OpencodeConn{
			{Kind: "credential", ID: "cred-3", Label: "OAuth", Method: "oauth", NeedsAuth: "token expired"},
		}},
	}
}

func rowsOfPage(p opencodePage) []tui.ListRow {
	return p.spec().Rows
}

func TestOpencodePageRows1x(t *testing.T) {
	d, _ := ocDialog()
	p := buildOpencodePage(d, v1Data(), 1)
	if p.title != "opencode  ✓ 5 accounts · opencode 1.x" {
		t.Errorf("title = %q", p.title)
	}
	want := []tui.ListRow{
		{Label: "Databricks", Detail: "API key", OK: true, Section: "accounts"},
		{Label: "GitHub Copilot", Detail: "OAuth", OK: true, Section: "accounts"},
		{Label: "Nvidia", Detail: "API key", OK: true, Section: "accounts"},
		{Label: "OpenAI", Detail: "OAuth", OK: true, Section: "accounts"},
		{Label: "zed", Detail: "API key", OK: true, Section: "accounts"},
		{Label: "+ Add account", Detail: "opencode auth login · browser sign-in", Section: "actions"},
	}
	if got := rowsOfPage(p); !reflect.DeepEqual(got, want) {
		t.Errorf("rows =\n%+v\nwant\n%+v", got, want)
	}
	spec := p.spec()
	if !spec.Search || len(spec.Binds) != 1 || spec.Binds[0].Name != "logout" || spec.Binds[0].Key.Name() != "ctrl+d" ||
		spec.Binds[0].Confirm != "Press ctrl+d again to confirm" {
		t.Errorf("spec search/binds = %v %+v", spec.Search, spec.Binds)
	}
}

func TestOpencodePageRows2x(t *testing.T) {
	d, _ := ocDialog()
	p := buildOpencodePage(d, v2Data(), 2)
	if p.title != "opencode  ✓ 4 accounts · opencode 2.x" {
		t.Errorf("title = %q", p.title)
	}
	got := rowsOfPage(p)
	want := []tui.ListRow{
		{Label: "GitHub Copilot", Detail: "work", Status: "active", OK: true, Section: "accounts"},
		{Label: "GitHub Copilot", Detail: "personal", OK: true, Section: "accounts"},
		{Label: "GitHub Copilot", Detail: "$GITHUB_TOKEN", Status: "environment · unset $GITHUB_TOKEN to disconnect", OK: true, Section: "accounts"},
		{Label: "OpenAI", Detail: "OAuth", Status: "sign in required", OK: false, Section: "accounts"},
	}
	if !reflect.DeepEqual(got[:4], want) {
		t.Errorf("rows =\n%+v\nwant\n%+v", got[:4], want)
	}
}

func TestOpencodePageNotConnected(t *testing.T) {
	d, _ := ocDialog()
	p := buildOpencodePage(d, nil, 0)
	if p.title != "opencode  not connected" {
		t.Errorf("title = %q", p.title)
	}
	if r := p.rows[0]; r.row.Label != "not connected" || r.run != nil || r.conn != nil {
		t.Errorf("first row = %+v", r)
	}
	f := &ocFake{answers: []tui.ListResult{{Index: 0}, {Index: 0, Action: "logout"}}}
	installOCFake(t, f)
	runOpencodePage(d)
	if len(f.ran) != 0 || len(f.specs) != 3 {
		t.Errorf("not-connected row ran %v (specs %d)", f.ran, len(f.specs))
	}
}

func TestOpencodePageEscReturns(t *testing.T) {
	f := &ocFake{ints: v1Data(), major: 1}
	installOCFake(t, f)
	d, out := ocDialog()
	runOpencodePage(d)
	if len(f.specs) != 1 || len(f.ran) != 0 || d.changed || out.Len() != 0 {
		t.Errorf("Esc: specs=%d ran=%v changed=%v out=%q", len(f.specs), f.ran, d.changed, out.String())
	}
}

func TestOpencodePageCtrlDLogout(t *testing.T) {
	for _, c := range []struct {
		major int
		ints  []vendors.OpencodeIntegration
		row   int
		want  []string
		name  string
	}{
		{1, v1Data(), 1, []string{"/real/opencode", "auth", "logout", "github-copilot"}, "GitHub Copilot"},
		{2, v2Data(), 1, []string{"/real/opencode", "auth", "logout", "github-copilot", "cred-2"}, "GitHub Copilot"},
	} {
		f := &ocFake{ints: c.ints, major: c.major, answers: []tui.ListResult{{Index: c.row, Action: "logout"}}}
		installOCFake(t, f)
		d, out := ocDialog()
		runOpencodePage(d)
		if len(f.ran) != 1 || !reflect.DeepEqual(f.ran[0], c.want) {
			t.Errorf("%d.x ran %v, want %v", c.major, f.ran, c.want)
		}
		if f.invalidated != 1 || !d.changed || !strings.Contains(out.String(), "Removed account from "+c.name) {
			t.Errorf("%d.x invalidated=%d changed=%v out=%q", c.major, f.invalidated, d.changed, out.String())
		}
		if len(f.specs) != 2 || len(f.asked) != 0 { // re-rendered after the action; ctrl+d needs no [y/N]
			t.Errorf("%d.x specs=%d asked=%v", c.major, len(f.specs), f.asked)
		}
	}
}

func TestOpencodePageLogoutCancelledAndError(t *testing.T) {
	f := &ocFake{ints: v1Data(), major: 1, cancel: true, answers: []tui.ListResult{{Index: 0, Action: "logout"}}}
	installOCFake(t, f)
	d, out := ocDialog()
	runOpencodePage(d)
	if d.changed || !strings.Contains(out.String(), "Databricks: logout cancelled") {
		t.Errorf("cancel: changed=%v out=%q", d.changed, out.String())
	}
	f2 := &ocFake{ints: v1Data(), major: 1, runErr: errors.New("exit status 1"), answers: []tui.ListResult{{Index: 0, Action: "logout"}}}
	installOCFake(t, f2)
	d2, out2 := ocDialog()
	runOpencodePage(d2)
	if d2.changed || !strings.Contains(out2.String(), "Databricks: exit status 1") {
		t.Errorf("error: changed=%v out=%q", d2.changed, out2.String())
	}
}

func TestOpencodePageEnvRowRefusesLogout(t *testing.T) {
	f := &ocFake{ints: v2Data(), major: 2, answers: []tui.ListResult{{Index: 2, Action: "logout"}, {Index: 2}}}
	installOCFake(t, f)
	d, out := ocDialog()
	runOpencodePage(d)
	if len(f.ran) != 0 || d.changed || len(f.specs) != 3 {
		t.Errorf("env row ran %v changed=%v specs=%d", f.ran, d.changed, len(f.specs))
	}
	if n := strings.Count(out.String(), "GitHub Copilot: comes from $GITHUB_TOKEN; unset it to disconnect"); n != 2 {
		t.Errorf("env message x%d: %q", n, out.String())
	}
}

// actionLabels runs Enter on row and returns the action page's labels (answering Back/Esc there).
func actionLabels(t *testing.T, major int, ints []vendors.OpencodeIntegration, row int) []string {
	t.Helper()
	f := &ocFake{ints: ints, major: major, answers: []tui.ListResult{{Index: row}, {Index: -1}}}
	installOCFake(t, f)
	d, _ := ocDialog()
	runOpencodePage(d)
	if len(f.specs) < 2 {
		t.Fatalf("no action page: %d specs", len(f.specs))
	}
	var labels []string
	for _, r := range f.specs[1].Rows {
		labels = append(labels, r.Label)
	}
	if f.specs[1].Search {
		t.Errorf("action page must not search")
	}
	return labels
}

func TestOpencodePageSwitchOfferedOnlyOn2xInactive(t *testing.T) {
	if got := strings.Join(actionLabels(t, 1, v1Data(), 1), ","); got != "Log out,Back" {
		t.Errorf("1.x actions = %s", got)
	}
	if got := strings.Join(actionLabels(t, 2, v2Data(), 0), ","); got != "Log out,Back" { // already active
		t.Errorf("2.x active actions = %s", got)
	}
	if got := strings.Join(actionLabels(t, 2, v2Data(), 1), ","); got != "Make active,Log out,Back" {
		t.Errorf("2.x inactive actions = %s", got)
	}
}

func TestOpencodePageMakeActive(t *testing.T) {
	f := &ocFake{ints: v2Data(), major: 2, answers: []tui.ListResult{{Index: 1}, {Index: 0}}}
	installOCFake(t, f)
	d, out := ocDialog()
	runOpencodePage(d)
	want := []string{"/real/opencode", "auth", "switch", "github-copilot", "cred-2"}
	if len(f.ran) != 1 || !reflect.DeepEqual(f.ran[0], want) {
		t.Errorf("ran %v, want %v", f.ran, want)
	}
	if !d.changed || f.invalidated != 1 || !strings.Contains(out.String(), "GitHub Copilot: personal is now active") {
		t.Errorf("changed=%v invalidated=%d out=%q", d.changed, f.invalidated, out.String())
	}
}

func TestOpencodePageActionLogOutConfirms(t *testing.T) {
	f := &ocFake{ints: v1Data(), major: 1, answers: []tui.ListResult{{Index: 3}, {Index: 0}}}
	installOCFake(t, f)
	d, _ := ocDialog()
	runOpencodePage(d)
	if len(f.ran) != 0 || len(f.asked) != 1 || f.asked[0] != "Log out of OpenAI (OAuth)?" {
		t.Errorf("declined: ran=%v asked=%v", f.ran, f.asked)
	}
	f2 := &ocFake{ints: v1Data(), major: 1, confirm: true, answers: []tui.ListResult{{Index: 3}, {Index: 0}}}
	installOCFake(t, f2)
	d2, out2 := ocDialog()
	runOpencodePage(d2)
	if len(f2.ran) != 1 || !reflect.DeepEqual(f2.ran[0], []string{"/real/opencode", "auth", "logout", "openai"}) ||
		!strings.Contains(out2.String(), "Removed account from OpenAI") {
		t.Errorf("confirmed: ran=%v out=%q", f2.ran, out2.String())
	}
}

func TestOpencodePageAddAccount(t *testing.T) {
	addRow := len(v1Data())
	f := &ocFake{ints: v1Data(), major: 1, cancel: true, answers: []tui.ListResult{{Index: addRow}, {Index: addRow, Action: "logout"}}}
	installOCFake(t, f)
	d, out := ocDialog()
	runOpencodePage(d)
	if len(f.ran) != 1 || !reflect.DeepEqual(f.ran[0], []string{"/real/opencode", "auth", "login"}) {
		t.Errorf("ran %v (ctrl+d on an action row must be ignored)", f.ran)
	}
	if d.changed || !strings.Contains(out.String(), "opencode: sign-in cancelled") {
		t.Errorf("cancel: changed=%v out=%q", d.changed, out.String())
	}

	f2 := &ocFake{ints: v1Data(), major: 1, answers: []tui.ListResult{{Index: addRow}}}
	installOCFake(t, f2)
	d2, out2 := ocDialog()
	runOpencodePage(d2)
	if !d2.changed || f2.invalidated != 1 || !strings.Contains(out2.String(), "opencode: 5 accounts") {
		t.Errorf("ok: changed=%v invalidated=%d out=%q", d2.changed, f2.invalidated, out2.String())
	}

	f3 := &ocFake{ints: v1Data(), major: 1, answers: []tui.ListResult{{Index: addRow}}}
	installOCFake(t, f3)
	lookOpencode = func(string) (string, error) { return "", errors.New("opencode: not found on PATH") }
	d3, out3 := ocDialog()
	runOpencodePage(d3)
	if len(f3.ran) != 0 || !strings.Contains(out3.String(), "not found on PATH") {
		t.Errorf("no real opencode must not run the shim: ran=%v out=%q", f3.ran, out3.String())
	}
}

func TestOpencodePageAdapterErrorStillRenders(t *testing.T) {
	f := &ocFake{ints: v1Data()[:1], major: 2, err: errors.New("opencode auth list: timeout"),
		answers: []tui.ListResult{{Index: 0}, {Index: -1}}} // Enter → action page → Esc → page again → Esc
	installOCFake(t, f)
	d, out := ocDialog()
	runOpencodePage(d)
	if n := strings.Count(out.String(), "opencode: opencode auth list: timeout"); n != 1 {
		t.Errorf("error printed x%d: %q", n, out.String())
	}
	if len(f.specs) != 3 || f.specs[0].Rows[0].Label != "Databricks" {
		t.Errorf("specs=%d rows=%+v", len(f.specs), f.specs[0].Rows)
	}
}

// TestOpencodeHomeRowOpensPage: the home row keeps opencodeMenu's state columns; its action opens the new page.
func TestOpencodeHomeRowOpensPage(t *testing.T) {
	reg := menuTestEnv(t)
	d := &dialog{reg: reg, out: discardWriter{}, o: Options{Vendor: "opencode"}}
	rows, actions := menuRows(d)
	if len(rows) != 1 || rows[0][0] != "opencode" {
		t.Fatalf("rows = %v", rows)
	}
	st := opencodeMenu(d).state
	if !strings.Contains(rows[0][1], modeLabel(d, "opencode", st)) || !strings.Contains(rows[0][1], st.check()) {
		t.Errorf("home row %q lost mode %q / check %q", rows[0][1], modeLabel(d, "opencode", st), st.check())
	}
	f := &ocFake{}
	installOCFake(t, f)
	title, _, _ := fakePickAgent(t, -1)
	actions[0](d)
	if len(f.specs) != 1 || !strings.HasPrefix(f.specs[0].Title, "opencode  not connected") || *title != "" {
		t.Fatalf("opencode row must open the list page, not runAgentMenu: specs=%d title=%q", len(f.specs), *title)
	}
	var labels []string
	for _, r := range f.specs[0].Rows {
		labels = append(labels, r.Label)
	}
	if got := strings.Join(labels, ","); !strings.HasPrefix(got, "not connected,+ Add account,Model") || strings.Contains(got, "API key") {
		t.Errorf("page rows = %s", got)
	}
}

func TestOpencodeDatabricksRowSetsUp(t *testing.T) {
	for _, c := range []struct {
		name     string
		profiles int
		picks    []int
		want     string
	}{
		{"one profile skips the picker", 1, []int{0}, "login:dev,mint:dev"},
		{"several profiles ask", 3, []int{1}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := dbxSetUpEnv(t)
			for _, n := range []string{"qa", "prod"}[:3-c.profiles] {
				delete(d.reg.Providers, n)
			}
			if c.want == "" { // the second listed profile is the one picked
				n := dbxProfileNames(d.reg)[1]
				c.want = "login:" + n + ",mint:" + n
			}
			df := &dbxFake{picks: c.picks}
			installDbxFake(t, df)
			f := &ocFake{ints: v1Data(), major: 1, answers: []tui.ListResult{{Index: 0}, {Index: 0}}}
			installOCFake(t, f)
			runOpencodePage(d)
			if got := strings.Join(df.calls, ","); got != c.want {
				t.Errorf("calls = %q, want %q", got, c.want)
			}
			picker := 0
			for _, ti := range df.titles {
				if ti == "Set up Databricks" {
					picker++
				}
			}
			if (picker == 1) != (c.profiles > 1) {
				t.Errorf("profile picker shown %d times with %d profiles", picker, c.profiles)
			}
			if strings.Contains(dbxOut(d), dbxTok) {
				t.Errorf("output leaked the token: %q", dbxOut(d))
			}
		})
	}
}
