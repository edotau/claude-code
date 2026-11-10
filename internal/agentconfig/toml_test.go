package agentconfig

import "testing"

// TOMLString must take the quoted value only: a trailing comment or literal quotes must not ride into the model id.
func TestTomlString(t *testing.T) {
	cases := map[string]string{
		`"gpt-6-astra"`:             "gpt-6-astra",
		`"gpt-6-astra" # preferred`: "gpt-6-astra",
		`'gpt-6-astra'`:             "gpt-6-astra",
		`gpt-6-astra # bare`:        "gpt-6-astra",
		`"unterminated`:             "unterminated",
	}
	for in, want := range cases {
		if got := TOMLString(in); got != want {
			t.Errorf("TOMLString(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInsertTopLevelTOMLQuotedReplacementKey(t *testing.T) {
	for _, key := range []string{`"model"`, `'model'`} {
		for _, suffix := range []string{"", "[profile]\nmodel = \"table\"\n"} {
			t.Run(key+suffix, func(t *testing.T) {
				section := key + " = \"new\"\n"
				want := section
				if suffix != "" {
					want += "\n" + suffix
				}
				got := InsertTopLevelTOML("model = \"old\"\n"+suffix, section)
				if got != want {
					t.Fatalf("quoted replacement must replace the top-level key and preserve table keys: got %q, want %q", got, want)
				}
			})
		}
	}
}
