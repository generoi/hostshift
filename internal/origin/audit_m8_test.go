package origin

import "testing"

// TestLoneSeparatorHalfYieldsNoCandidate. nextSep returns sepLen = -1 to mean
// "this \/ or %2F is a lone half, not the start of a separator". fill() added it
// as an offset anyway, probing for a host one byte *before* the match — where a
// one-byte canonical host matches, producing a zero-width candidate the
// automaton never yields.
//
// With an alphanumeric one-byte host the phantom is only counted, inflating the
// --explain candidate totals §4.4 asks a reader to trust. With a
// non-alphanumeric one it survives the anchor check and splices bytes into a
// response the automaton would have left alone.
func TestLoneSeparatorHalfYieldsNoCandidate(t *testing.T) {
	for _, tc := range []struct {
		name, canonical, variant, in string
	}{
		{"json half, one-byte host", "https://a", "https://z", `a\/`},
		{"percent half, one-byte host", "https://a", "https://z", "aa%2F"},
		{"json half splices", "https://+", "https://z.z", `q+\/x`},
		{"percent half splices", "https://+", "https://z.z", "+%2F"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse(tc.canonical)
			if err != nil {
				t.Skipf("%s is not a parseable origin: %v", tc.canonical, err)
			}
			v, err := Parse(tc.variant)
			if err != nil {
				t.Fatal(err)
			}
			m, err := NewMap([]Site{{Name: "s", Canonical: c, Variant: v}})
			if err != nil {
				t.Fatal(err)
			}
			out, events := m.Forward().Rewrite([]byte(tc.in), "text", true)
			if string(out) != tc.in {
				t.Errorf("rewrote %q to %q; a lone separator half is not a separator", tc.in, out)
			}
			if len(events) != 0 {
				t.Errorf("%q yielded %d candidate(s); the automaton yields none: %+v", tc.in, len(events), events)
			}
		})
	}
}
