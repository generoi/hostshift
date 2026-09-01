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
