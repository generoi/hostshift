package rewrite

import (
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

func viewMap(t *testing.T) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://v.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The views were siblings, never a stack: each decoded the raw value its own way
// and none ever saw another's output. So a spelling needing *two* decodes went
// out byte-identical with the census calling the page clean.
//
// A real Chrome fetches every positive case here: HTML decodes `&#92;` to a
// backslash in the attribute value, and the CSS tokenizer then unescapes `\3a `
// — getAttribute('style') returns exactly what the plain spelling gives. Neither
// view alone can see it, because stripForCSS's `\` guard is false on the
// still-encoded bytes and stripForRefs decodes to a `\3a ` no reference view
// unescapes.
//
// The negative case is the control, and it is the interesting half: an HTML
// `<style>` element is raw text, so its references are *not* decoded, and Chrome
// does not fetch it. Rewriting it would be the mirror error — a decode the
// consumer never performs.
func TestReferencesSpellingCSSEscapes(t *testing.T) {
	m := viewMap(t)
	const ref = `https&#92;3a &#92;2f &#92;2f www.example.fi&#92;2f x.png`

	for _, c := range []struct {
		name, in string
		rewrite  bool
	}{
		{"a style attribute decodes both",
			`<div style="background:url(` + ref + `)">t</div>`, true},
		{"a style element inside svg is foreign content, so it does too",
			`<svg><style>#d{background:url(` + ref + `)}</style></svg>`, true},
		{"an HTML style element decodes neither",
			`<style>#d{background:url(` + ref + `)}</style>`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := NewStats(false)
			out := rewriteHTML(t, m, c.in, st)
			if got := out != c.in; got != c.rewrite {
				t.Fatalf("rewrote=%v, want %v:\n in  %s\n out %s", got, c.rewrite, c.in, out)
			}
			if c.rewrite && strings.Contains(out, "www.example.fi") {
				t.Errorf("a canonical origin survives:\n%s", out)
			}
			// And whatever it did, the census has to say so — see
			// TestEveryViewReportsToTheCensus.
			if c.rewrite && st.Total() == 0 {
				t.Error("rewritten, but the census counted nothing")
			}
		})
	}
}

// A standalone SVG is the same content with no HTML parser above it, and
// HostLeaksXML is a different entry point, so it gets the composed view too.
func TestComposedViewReachesStandaloneXML(t *testing.T) {
	m := viewMap(t)
	in := []byte(`<svg><style>#d{background:url(https&#92;3a &#92;2f &#92;2f www.example.fi/x.png)}</style></svg>`)
	out := string(HostLeaksXML(m, in, false))
	if strings.Contains(out, "www.example.fi") {
		t.Errorf("a canonical origin survives a standalone SVG:\n%s", out)
	}
}

// An instrument reporting health it did not measure — the failure the PLAN
// records twice and this is the third instance of.
//
// spliceHostsIn recorded nothing, and every view but the reference one went
// through it. §5.8 makes `--dry-run` the mode you point at a canonical checkout
// to decide whether a site needs hostshift at all, and on a page whose origins
// are all percent- or CSS-encoded it printed zero rewrites and zero candidates:
// "nothing to do", on the very WooCommerce shape stripForPercent was written
// for. countLeaks then fell back to `changed → 1`, so forty leaks read as one.
func TestEveryViewReportsToTheCensus(t *testing.T) {
	m := viewMap(t)
	for _, c := range []struct{ name, in string }{
		{"the percent view",
			`<script>JSON.parse(decodeURIComponent("https%3A%5C%2F%5C%2Fwww.example.fi%2Fx"))</script>`},
		{"the css view",
			`<div style="background:url(https\3a \2f \2f www.example.fi/h.jpg)">x</div>`},
		{"the composed refs-then-css view",
			`<div style="background:url(https&#92;3a &#92;2f &#92;2f www.example.fi/h.jpg)">x</div>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := NewStats(false)
			out := rewriteHTML(t, m, c.in, st)
			if out == c.in {
				t.Fatalf("nothing was rewritten, so this asserts nothing:\n%s", out)
			}
			if st.Total() == 0 {
				t.Errorf("%d bytes changed and the census reports nothing:\n%s",
					len(out)-len(c.in), out)
			}
		})
	}
}

// A trailing dot is the host's root label in a URL and a full stop in prose, and
// the distinction has to survive every encoding — the percent view hardcoded
// value=true, so it alone ate the sentence's punctuation, and authorityEnd
// re-widened to the whole authority, undoing the carve-out whenever the scheme
// or port arm fired.
//
// The last case is the counterexample that keeps the carve-out honest: a full
// stop is never followed by `:80`, so there the dot is the root label whatever
// the surface. Dropping it split the host from its port, the splice replaced
// neither, and the forward pass emitted a variant the reverse could not read.
func TestATrailingDotIsPunctuationOnlyWhenItEndsTheAuthority(t *testing.T) {
	m := viewMap(t)
	for _, c := range []struct{ name, in, want string }{
		{"plain", `<p>See https://www.example.fi. Thanks</p>`,
			`<p>See https://v.ddev.site. Thanks</p>`},
		{"json-escaped", `<p>See https:\/\/www.example.fi. Thanks</p>`,
			`<p>See https:\/\/v.ddev.site. Thanks</p>`},
		{"percent-encoded", `<p>See https%3A%5C%2F%5C%2Fwww.example.fi. Thanks</p>`,
			`<p>See https%3A%5C%2F%5C%2Fv.ddev.site. Thanks</p>`},
		{"a differing scheme, which takes the scheme arm",
			`<p>See http:www.example.fi. Thanks</p>`,
			`<p>See https://v.ddev.site. Thanks</p>`},
		// :80 rather than :443, because http://host:443 is genuinely a
		// different origin and §5.4 matches on exact origin equality — the
		// bare-host fallback fires only when the port is the scheme's default.
		{"but a port means the dot is the root label",
			`<p>See http:www.example.fi.:80 ok</p>`,
			`<p>See https://v.ddev.site ok</p>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if out := rewriteHTML(t, m, c.in, NewStats(false)); out != c.want {
				t.Errorf("\n got  %s\n want %s", out, c.want)
			}
		})
	}
}

// The JSON path had one reference path where the HTML side has three, and it was
// the one that declines: decodeURLRefs refuses an entire value when any fragment
// in it would fuse into a new reference, so a `&#6`+`&#48;`+`;` anywhere in a
// query string disabled decoding for an ordinary `https:&#47;&#47;canonical/` in
// the same string.
//
// `content.rendered` is injected into the page as HTML, so that href is a live
// production link — the exact asymmetry decodeJSONLeak's own header exists to
// name: the page rewrites, the REST API does not.
func TestJSONReadsReferencesTheBrowserDecodes(t *testing.T) {
	m := viewMap(t)
	for _, in := range []string{
		`{"rendered":"<a href=\"https:&#47;&#47;www.example.fi/x\">t</a>"}`,
		`{"rendered":"<a href=\"https:&#47;&#47;www.example.fi/x?q=&#6&#48;;\">t</a>"}`,
	} {
		t.Run(in, func(t *testing.T) {
			out := string(RewriteJSON([]byte(in), m, NewStats(false), nil, false))
			if strings.Contains(out, "www.example.fi") {
				t.Errorf("a canonical origin survives the REST body:\n%s", out)
			}
		})
	}
}
