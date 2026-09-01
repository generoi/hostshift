package rewrite

import (
	"runtime"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 49, on acc0ae8.

func r49Map(t *testing.T, canon, variant string) *origin.Map {
	t.Helper()
	m, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse(canon),
		Variant:   origin.MustParse(variant),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestR49TheSchemeArmStillSplicesPunycode.
//
// acc0ae8 changed two of locateHostIn's three return arms to DisplayHostPort().
// The third — `case needScheme`, the one that rewrites the whole origin because
// the target's scheme differs from the one written — still returns to.String(),
// which is built on HostPort() and is therefore the ACE form.
func TestR49TheSchemeArmStillSplicesPunycode(t *testing.T) {
	m := r49Map(t, "https://www.hämeenlinna.fi", "https://wt-a--hml.ddev.site")

	for _, in := range []string{
		`http://wt-a--hml.ddev.site/uutiset`,
		`http:\/\/wt-a--hml.ddev.site\/uutiset`,
		`http%3A%2F%2Fwt-a--hml.ddev.site%2Fuutiset`,
	} {
		out := string(HostLeaksBack(m.Reverse(), []byte(in)))
		if strings.Contains(out, "xn--") {
			t.Errorf("needScheme arm spliced punycode:\n  in:  %s\n  out: %s", in, out)
		}
		if !strings.Contains(out, "hämeenlinna") {
			t.Errorf("declared spelling lost:\n  in:  %s\n  out: %s", in, out)
		}
	}
}

// TestR49EachDirectionIsAFixedPointWithAnIDNMap: the replacement table now
// carries a U-label, so a second pass sees bytes the first pass wrote. Passes on
// acc0ae8; here because nothing else asks it of the Display path.
func TestR49EachDirectionIsAFixedPointWithAnIDNMap(t *testing.T) {
	m := r49Map(t, "https://www.hämeenlinna.fi", "https://wt-a--hml.ddev.site")
	for _, c := range []struct {
		name string
		mm   *origin.Matcher
		in   string
	}{
		{"forward", m.Forward(), `https:\/\/www.hämeenlinna.fi\/x <a href="https://www.hämeenlinna.fi/y">`},
		{"reverse", m.Reverse(), `https:\/\/wt-a--hml.ddev.site\/x <a href="https://wt-a--hml.ddev.site/y">`},
	} {
		once := HostLeaks(c.mm, []byte(c.in), false)
		twice := HostLeaks(c.mm, once, false)
		if string(once) != string(twice) {
			t.Errorf("%s is not a fixed point:\n  1x: %s\n  2x: %s", c.name, once, twice)
		}
	}
	// And a body already carrying the U-label canonical survives a reverse pass.
	body := "https://www.hämeenlinna.fi/x"
	if out := string(HostLeaksBack(m.Reverse(), []byte(body))); out != body {
		t.Errorf("the reverse pass changed a body already in canonical form:\n  %s", out)
	}
}

// TestR49ThePortArmOfTheDeclaredSpellingHasNoTest passes on acc0ae8. It is here
// because reverting that arm — `case hasPort:` back to to.HostPort() — makes no
// existing test in the repository fail, so half of the locator's share of §5.5
// was silently revertible. The other two arms are covered (r48's test pins one,
// TestR49TheSchemeArmStillSplicesPunycode shows the third was never changed).
func TestR49ThePortArmOfTheDeclaredSpellingHasNoTest(t *testing.T) {
	m := r49Map(t, "https://www.hämeenlinna.fi:8443", "https://wt-a--hml.ddev.site:8443")
	in := `https:\/\/wt-a--hml.ddev.site:8443\/uutiset`
	out := string(HostLeaksBack(m.Reverse(), []byte(in)))
	if strings.Contains(out, "xn--") || !strings.Contains(out, "hämeenlinna.fi:8443") {
		t.Errorf("the port arm did not emit the declared spelling:\n  in:  %s\n  out: %s", in, out)
	}
}

// TestR49OneOctalDigitArmsTheJSONEscapeView.
//
// hasJSONEsc's gate was a two-byte needle on purpose: "every view in this family
// builds three slices the length of the body ... that took the composite from
// 200x to 304x once already". acc0ae8 widened it to `\` followed by any of
// 0-7, LF or CR — which jsEscAt's own comment says must not happen, naming "a
// regex backreference, a Windows path" as the reason. One such byte anywhere in
// an 8 MiB body now arms stripForJSONEsc, and (through hasRefJSONEsc, which
// calls hasJSONEsc first) the refs∘JSONEsc composed view too.
func TestR49OneOctalDigitArmsTheJSONEscapeView(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation")
	}
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--example.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	measure := func(b []byte) float64 {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		HostLeaksBack(m, b)
		runtime.ReadMemStats(&after)
		return float64(after.TotalAlloc-before.TotalAlloc) / float64(len(b))
	}
	body := func(needle string) []byte {
		b := []byte(strings.Repeat("&", 8<<20))
		copy(b[len(b)/2:], []byte(needle))
		return b
	}
	// Both bodies carry one backslash, so the CSS view — gated on a bare
	// backslash and unchanged by acc0ae8 — fires for both. The only difference
	// is whether the byte after it is an octal digit, which is what hasJSONEsc
	// now keys on. `\3a` is a CSS escape for a colon; it is the token
	// r46CompositeUnit itself is built from, and `\2014`, `\00a0` and `\003e`
	// are the same shape on any page with a themed quote or a non-breaking
	// space in a stylesheet.
	notOctal := measure(body(`\ba `))
	octal := measure(body(`\3a `))

	t.Logf("one `\\ba ` in 8 MiB of ampersands: %.0fx the body; one `\\3a `: %.0fx",
		notOctal, octal)
	if octal > notOctal*1.2 {
		t.Errorf("one CSS escape starting with an octal digit took allocation from "+
			"%.0fx to %.0fx the body, past the 128x this shape is bounded at; "+
			"no fixture in TestAllocationStaysBounded measures it", notOctal, octal)
	}
}

// TestR49LineContinuationInventsAHost.
//
// `\` + newline is removed by stripForJSONEsc, joining the two halves. That is
// the JavaScript rule, but this view is applied to HTML attribute values, plain
// text and request bodies too, where the WHATWG URL parser instead treats the
// backslash as a `/` — so `https://www.example.f\<LF>i/x` has the host
// `www.example.f` in a browser and `www.example.fi` here.
//
// Pre-existing, not new at acc0ae8: stripForCSS's "an escaped literal stands for
// itself" arm already emitted the newline and stripRemovals already deleted it,
// so the CSS view reached the same join. acc0ae8 added a second route to it.
//
// Open, and pinned here rather than fixed. Round 49 removed the JSON view's
// route to this join; the CSS view's is older and is not removable without
// answering a question this design has not had to answer yet — `rewriteAll`
// applies every view to every surface deliberately, because the reverse
// direction must read whatever the forward direction can emit, and "what does a
// backslash mean here" has a different answer in CSS, in JS and in an HTML
// attribute. The cost of the join is a wrong rewrite on a shape nobody has
// produced; the cost of removing the CSS route is a real CSS escape going
// unread. Recorded in PLAN §5.2.
//
// This asserts today's behaviour so the gap stays visible and so a fix shows up
// as a failure here rather than as silence.
func TestR49LineContinuationInventsAHost(t *testing.T) {
	m := r49Map(t, "https://www.example.fi", "https://wt-a--example.ddev.site")
	in := "https://www.example.f\\\ni/x"
	out := string(HostLeaks(m.Forward(), []byte(in), false))
	if out == in {
		t.Skip("the CSS view no longer joins across a line continuation — " +
			"if that was deliberate, invert this test and update PLAN §5.2")
	}
	if !strings.Contains(out, "wt-a--example.ddev.site") {
		t.Errorf("expected the documented (wrong) join, got something else:\n  %q", out)
	}
}

// TestR49DeclaringTheALabelRewritesItToUnicode.
//
// Origin.Display is documented as "the host as it was declared". It is computed
// with foldHost, whose ToUnicode *decodes* an A-label — so declaring the ACE
// form yields a Display that is the U-label, and the reverse pass then writes a
// spelling into the production database that the site never used.
func TestR49DeclaringTheALabelRewritesItToUnicode(t *testing.T) {
	m := r49Map(t, "https://www.xn--hmeenlinna-q5a.fi", "https://wt-a--hml.ddev.site")

	in := "see https://wt-a--hml.ddev.site/uutiset"
	out, _ := m.Reverse().RewriteText([]byte(in), "text", false)
	if !strings.Contains(string(out), "www.xn--hmeenlinna-q5a.fi") {
		t.Errorf("the A-label was declared but is not what came back:\n%s", out)
	}
}
