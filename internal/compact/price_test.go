package compact

import "testing"

func TestCost(t *testing.T) {
	cases := []struct {
		model         string
		tokens        int64
		rewrite, read int64
	}{
		{"claude-sonnet-5-5", 1_000_000, 4_000_000, 100_000}, // чтение 0.05× входа $2 — $0.10, не $0.20
		{"claude-opus-5-5[1m]", 200_000, 1_600_000, 40_000},
		{"no-such-model", 100_000, 0, 0},
	}
	for _, c := range cases {
		if w, r := cost(c.model, c.tokens); w != c.rewrite || r != c.read {
			t.Errorf("cost(%s, %d) = %d, %d; want %d, %d", c.model, c.tokens, w, r, c.rewrite, c.read)
		}
	}
}
