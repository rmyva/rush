package cmd

import "testing"

func TestEscapeSJSONPathComponent_SingleBackslash(t *testing.T) {
	for _, input := range []string{`provider\id`, `api\key`} {
		t.Run(input, func(t *testing.T) {
			got := escapeSJSONPathComponent(input)
			want := `provider\\id`
			if input == `api\key` {
				want = `api\\key`
			}
			if got != want {
				t.Errorf("escapeSJSONPathComponent(%q) = %q, want %q", input, got, want)
			}
		})
	}
}
