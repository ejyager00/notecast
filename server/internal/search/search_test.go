package search

import "testing"

func TestQuery(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"  ;; ":                "",
		"glyco":                `"glyco"*`,
		"Krebs cycle":          `"krebs" "cycle"*`,
		`taenia" OR NEAR(x) *`: `"taenia" "or" "near" "x"*`,
		"Chédiak-Higashi":      `"chédiak" "higashi"*`,
		"GPIb/V/IX":            `"gpib" "v" "ix"*`,
	}
	for in, want := range cases {
		if got := Query(in); got != want {
			t.Errorf("Query(%q) = %q, want %q", in, got, want)
		}
	}
}
