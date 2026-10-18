package configure

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/googlekeys"
	"github.com/edotau/claude-code/internal/secrets"
)

func TestGeminiMenuRowLabels(t *testing.T) {
	reg := menuTestEnv(t)
	m := geminiMenu(&dialog{reg: reg, out: discardWriter{}})
	var got []string
	for _, r := range m.rows {
		got = append(got, r.label)
	}
	if want := "Subscription,API key,Login,Mint key"; strings.Join(got, ",") != want {
		t.Errorf("gemini rows = %v, want %s", got, want)
	}
	if m.state.mode != modeNone {
		t.Errorf("state = %+v, want %s with nothing configured", m.state, modeNone)
	}
}

// fakeKeysEnv wires googlekeys at an httptest server and a temp ADC file; it records deletes.
func fakeKeysEnv(t *testing.T, lookupName string) (deleted *[]string) {
	t.Helper()
	var dels []string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"access_token":"tok"}`)) })
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/keys:lookupKey":
			if lookupName == "" {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"error":{"message":"not found"}}`))
				return
			}
			w.Write([]byte(`{"name":"` + lookupName + `"}`))
		case r.Method == http.MethodPost:
			w.Write([]byte(`{"name":"operations/c","done":true,"response":{"name":"projects/1/locations/global/keys/new"}}`))
		case r.Method == http.MethodDelete:
			dels = append(dels, strings.TrimPrefix(r.URL.Path, "/v2/"))
			w.Write([]byte(`{"name":"operations/d","done":true,"response":{}}`))
		case strings.HasSuffix(r.URL.Path, "/keyString"):
			w.Write([]byte(`{"keyString":"AIza-minted"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	oldTok, oldAPI := googlekeys.TokenURL, googlekeys.APIKeysURL
	googlekeys.TokenURL, googlekeys.APIKeysURL = srv.URL+"/token", srv.URL+"/v2"
	t.Cleanup(func() { googlekeys.TokenURL, googlekeys.APIKeysURL = oldTok, oldAPI })

	adc := filepath.Join(t.TempDir(), "adc.json")
	body := `{"type":"authorized_user","client_id":"c","client_secret":"s","refresh_token":"r","quota_project_id":"proj"}`
	if err := os.WriteFile(adc, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", adc)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	return &dels
}

// answerConfirms replaces actionConfirmed with a scripted answer per question prefix and records the questions.
func answerConfirms(t *testing.T, answers map[string]bool) *[]string {
	t.Helper()
	orig := actionConfirmed
	t.Cleanup(func() { actionConfirmed = orig })
	var asked []string
	actionConfirmed = func(q string) bool {
		asked = append(asked, q)
		for prefix, yes := range answers {
			if strings.HasPrefix(q, prefix) {
				return yes
			}
		}
		return false
	}
	return &asked
}

func fileKeyEnv(t *testing.T, current string) {
	t.Helper()
	menuTestEnv(t) // temp config/HOME and an empty PATH: no keyring tool, so the file plane
	if current != "" {
		if _, err := secrets.Store(geminiKeyEnv, current, true); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMintGeminiKeyStoresAndDeletesOldWhenConfirmed(t *testing.T) {
	fileKeyEnv(t, "AIza-old")
	dels := fakeKeysEnv(t, "projects/1/locations/global/keys/old")
	asked := answerConfirms(t, map[string]bool{"Mint": true, "Delete the old key": true})
	d := &dialog{out: discardWriter{}}
	if !mintGeminiKey(d) {
		t.Fatal("mintGeminiKey = false")
	}
	if v, src, _ := secrets.Get(geminiKeyEnv); v != "AIza-minted" || src != "file" {
		t.Errorf("stored key = %q from %q, want AIza-minted from file", v, src)
	}
	if len(*dels) != 1 || (*dels)[0] != "projects/1/locations/global/keys/old" {
		t.Errorf("deleted = %v, want the old key once", *dels)
	}
	if len(*asked) != 2 {
		t.Errorf("asked %v, want mint then delete", *asked)
	}
}

func TestMintGeminiKeyKeepsOldWhenDeleteDeclined(t *testing.T) {
	fileKeyEnv(t, "AIza-old")
	dels := fakeKeysEnv(t, "projects/1/locations/global/keys/old")
	answerConfirms(t, map[string]bool{"Mint": true, "Delete the old key": false})
	if !mintGeminiKey(&dialog{out: discardWriter{}}) {
		t.Fatal("mintGeminiKey = false")
	}
	if v, _, _ := secrets.Get(geminiKeyEnv); v != "AIza-minted" {
		t.Errorf("stored key = %q, want the minted one", v)
	}
	if len(*dels) != 0 {
		t.Errorf("deleted %v despite a No", *dels)
	}
}

func TestMintGeminiKeyUnresolvableOldKeyIsNotOffered(t *testing.T) {
	fileKeyEnv(t, "AIza-aistudio")
	dels := fakeKeysEnv(t, "") // lookup 404: key from another project
	asked := answerConfirms(t, map[string]bool{"Mint": true})
	if !mintGeminiKey(&dialog{out: discardWriter{}}) {
		t.Fatal("mintGeminiKey = false")
	}
	if len(*dels) != 0 || len(*asked) != 1 {
		t.Errorf("deleted=%v asked=%v, want no delete offer", *dels, *asked)
	}
}

func TestMintGeminiKeyDeclinedMintChangesNothing(t *testing.T) {
	fileKeyEnv(t, "AIza-old")
	fakeKeysEnv(t, "projects/1/locations/global/keys/old")
	answerConfirms(t, nil)
	if mintGeminiKey(&dialog{out: discardWriter{}}) {
		t.Fatal("mintGeminiKey = true after No")
	}
	if v, _, _ := secrets.Get(geminiKeyEnv); v != "AIza-old" {
		t.Errorf("key changed to %q", v)
	}
}

func TestMintGeminiKeyRefusesEnvPlane(t *testing.T) {
	menuTestEnv(t)
	t.Setenv(geminiKeyEnv, "AIza-from-env")
	asked := answerConfirms(t, map[string]bool{"Mint": true})
	var out strings.Builder
	if mintGeminiKey(&dialog{out: &out}) {
		t.Fatal("mintGeminiKey = true on the env plane")
	}
	if len(*asked) != 0 || !strings.Contains(out.String(), "process environment") {
		t.Errorf("asked=%v out=%q, want a refusal before any confirm", *asked, out.String())
	}
}
