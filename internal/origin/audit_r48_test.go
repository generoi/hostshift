package origin

import (
	"strings"
	"testing"
)

// TestR48TheMatcherEmitsTheDeclaredSpelling: the automaton's replacement table
// is the other half of §5.5's "preserve the original form on output".
//
// `hostReplacer` covers the locator's path; this covers `RewriteText`, which is
// what the proxy runs over a text/plain body and what the sweep goes through.
// Both had to change, and only one of them had a test — so the matcher half was
// revertible without anything failing.
func TestR48TheMatcherEmitsTheDeclaredSpelling(t *testing.T) {
	m, err := NewMap([]Site{{
		Name:      "hml",
		Canonical: MustParse("https://www.hämeenlinna.fi"),
		Variant:   MustParse("https://wt-a--hml.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	in := []byte("see https://wt-a--hml.ddev.site/uutiset for more")
	out, _ := m.Reverse().RewriteText(in, "text", false)

	if !strings.Contains(string(out), "www.hämeenlinna.fi") {
		t.Errorf("the reverse direction did not emit the declared spelling:\n%s", out)
	}
	if strings.Contains(string(out), "xn--") {
		t.Errorf("the reverse direction emitted punycode into the body:\n%s", out)
	}

	// Forward is unaffected: a variant is ASCII by construction, and the
	// canonical is still matched in either spelling.
	for _, src := range []string{
		"https://www.hämeenlinna.fi/x",
		"https://www.xn--hmeenlinna-q5a.fi/x",
	} {
		fwd, _ := m.Forward().RewriteText([]byte(src), "text", false)
		if !strings.Contains(string(fwd), "wt-a--hml.ddev.site") {
			t.Errorf("%s was not matched forward: %s", src, fwd)
		}
	}
}

// TestR49APercentContextKeepsTheRoundTrip: the declared spelling goes back in
// raw, and that is a choice with a cost on each side.
//
// Percent-encoding the U-label would be the more correct URL — raw UTF-8 in a
// request line is a spec violation, although nginx accepts it, and every other
// encoding in this engine matches the spelling it found. But the replacement
// table is static: by the time the reverse pass runs, the forward pass has
// already put an ASCII variant where the host was, so nothing records whether
// the original was written raw or percent-encoded. One spelling has to be
// chosen, and the round trip is what §4.3 is about — the database holds the raw
// U-label, because WordPress does not punycode `siteurl`.
func TestR49APercentContextKeepsTheRoundTrip(t *testing.T) {
	m, err := NewMap([]Site{{
		Name:      "hml",
		Canonical: MustParse("https://www.hämeenlinna.fi"),
		Variant:   MustParse("https://wt-a--hml.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	in := []byte("redirect_to=https%3A%2F%2Fwww.hämeenlinna.fi%2Fwp-admin%2F")
	fwd, _ := m.Forward().RewriteText(in, "text", false)
	back, _ := m.Reverse().RewriteText(fwd, "text", false)
	if string(back) != string(in) {
		t.Errorf("the round trip did not restore the bytes:\n  in:   %s\n  back: %s", in, back)
	}
}
