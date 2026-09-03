package rewrite

import (
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Every spelling the forward direction can emit, the reverse direction must be
// able to read.
//
// cssEscapeLeak splices the host into the *escaped* spelling, so a style
// attribute reaches the browser as `url(https\3a \2f \2f <variant>/x)` — and the
// editor posts that back. Nothing on the way in could read it: the byte
// matcher's prefilter needs `//`, `\/` or `%2F` and that string has none, and
// stripForCSS was reachable only from the forward pass. The variant hostname
// went upstream and into the database §4.3 says stays byte-identical to
// production, which the file's own comment calls worse than a leak.
func TestForwardEmissionsAreReadableInReverse(t *testing.T) {
	canon := origin.MustParse("https://www.example.fi")
	variant := origin.MustParse("https://wt-a--example.ddev.site")
	fwd, err := origin.NewMatcher([]origin.Pair{{Canonical: canon, Variant: variant}})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := origin.NewMatcher([]origin.Pair{{Canonical: variant, Variant: canon}})
	if err != nil {
		t.Fatal(err)
	}

	// Shapes a page can hold, each of which the forward pass rewrites in place.
	for _, in := range []string{
		`<div style="background:url(https\3a \2f \2f www.example.fi/hero.jpg)">x</div>`,
		`<div style="background:url(https\3a\2f\2fwww.example.fi/hero.jpg)">x</div>`,
		`<style>a{background:url("https\3a \2f \2f www.example.fi/a.png")}</style>`,
		`<a href="https:\\www.example.fi/x">y</a>`,
		`<a href="https://u@www.example.fi/x">y</a>`,
		`<a href="http:www.example.fi/x">y</a>`,
		// The reference spellings. refsOnly splices the host into the *encoded*
		// form, so the page carries `https:&#47;&#47;<variant>/x` — and the
		// reverse direction had only one reference path where the HTML side has
		// three, the one that declines a whole value when a fragment would fuse.
		`<a href="https:&#47;&#47;www.example.fi/x">y</a>`,
		`<a href="https:&#47;&#47;www.example.fi/x?a=&#6&#48;;b">y</a>`,
		`<a href="https&colon;&sol;&sol;www.example.fi/x">y</a>`,
		// And references spelling CSS escapes, which needs both decodes at once.
		`<div style="background:url(https&#92;3a &#92;2f &#92;2f www.example.fi/h.jpg)">x</div>`,
		// Percent, which is what percent-encoding a JSON-escaped URL produces.
		`<script>f(decodeURIComponent("https%3A%5C%2F%5C%2Fwww.example.fi%2Fx"))</script>`,
	} {
		t.Run(in, func(t *testing.T) {
			served := rewriteHTML(t, fwd, in, NewStats(false))
			if strings.Contains(served, canon.Host) {
				t.Fatalf("the forward pass left a canonical origin:\n%s", served)
			}
			// HostLeaksBack, which is what the proxy uses on a request body:
			// the response direction must not invent decodes a plain-text
			// consumer does not perform, but the request direction has to read
			// every spelling the response direction can emit.
			back := string(HostLeaksBack(rev, []byte(served)))
			if strings.Contains(back, variant.Host) {
				t.Errorf("a variant hostname survives the request direction, so it "+
					"would be written into the shared database:\n served %s\n back   %s",
					served, back)
			}
		})
	}
}
