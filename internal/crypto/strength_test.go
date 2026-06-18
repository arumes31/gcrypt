package crypto

import "testing"

func TestCheckPassphraseStrength(t *testing.T) {
	cases := []struct {
		name string
		pass string
		ok   bool
	}{
		{"too short", "Ab1xy", false},
		{"long but single class lowercase", "abcdefghijkl", false},
		{"single repeated char", "aaaaaaaaaaaa", false},
		{"common weak", "password1234", false},
		{"good mixed letters+digits", "correcthorse7", true},
		{"good letters+symbols", "river-stone-sky!", true},
		{"good upper+lower+digit", "BlueRiver2026", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckPassphraseStrength(c.pass)
			if c.ok && err != nil {
				t.Fatalf("expected %q to be accepted, got: %v", c.pass, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("expected %q to be rejected, but it passed", c.pass)
			}
		})
	}
}
