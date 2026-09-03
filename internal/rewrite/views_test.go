package rewrite

import (
	"bytes"
	"os"
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
		// A script inside <svg> decodes references but is not CSS, so the
		// escapes stay escaped and no browser resolves this. Wiring the
		// composed view to that surface would be the mirror error — rewriting
		// where the consumer performs no decode.
		{"a script inside svg decodes references but is not CSS",
			`<svg><script>f("` + ref + `")</script></svg>`, false},
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

// The shapes the byte-identity corpus cannot hold, pinned by exact expectation
// instead.
//
// A root dot, uppercase letters, U+3002, a differing scheme and a default port
// are all rewritten to a canonical spelling rather than back to their original
// bytes. That is inside the host/port byte range §4.3 permits to change, and it
// is why these cannot sit in spike/adv with the round-trip fixtures — but it
// still has to be *stated*, because "the round trip is lossy here" is exactly
// the kind of claim that quietly becomes "the round trip is broken here".
//
// What must hold: the browser is served the plain variant, nothing carries a
// canonical origin, and a second round trip changes nothing more.
func TestNormalisingShapesReachAFixedPoint(t *testing.T) {
	fwd := pairMatcher(t, "https://www.acmecorp.fi", "https://wt-a--acmecorp.ddev.site")
	rev := pairMatcher(t, "https://wt-a--acmecorp.ddev.site", "https://www.acmecorp.fi")

	for _, c := range []struct{ in, served, back string }{
		{`<a href="https://www.acmecorp.fi./x">t</a>`,
			`<a href="https://wt-a--acmecorp.ddev.site/x">t</a>`,
			`<a href="https://www.acmecorp.fi/x">t</a>`},
		{`<a href="https://WWW.ACMECORP.FI/x">t</a>`,
			`<a href="https://wt-a--acmecorp.ddev.site/x">t</a>`,
			`<a href="https://www.acmecorp.fi/x">t</a>`},
		{`<a href="https://www.acmecorp.fi。/x">t</a>`,
			`<a href="https://wt-a--acmecorp.ddev.site/x">t</a>`,
			`<a href="https://www.acmecorp.fi/x">t</a>`},
		{`<a href="http:www.acmecorp.fi/x">t</a>`,
			`<a href="https://wt-a--acmecorp.ddev.site/x">t</a>`,
			`<a href="https://www.acmecorp.fi/x">t</a>`},
		{`<a href="https://www.acmecorp.fi:443/x">t</a>`,
			`<a href="https://wt-a--acmecorp.ddev.site/x">t</a>`,
			`<a href="https://www.acmecorp.fi/x">t</a>`},
	} {
		t.Run(c.in, func(t *testing.T) {
			served := rewriteHTML(t, fwd, c.in, NewStats(false))
			if served != c.served {
				t.Errorf("served:\n got  %s\n want %s", served, c.served)
			}
			back := rewriteHTML(t, rev, served, NewStats(false))
			if back != c.back {
				t.Errorf("back:\n got  %s\n want %s", back, c.back)
			}
			// Lossy once is a normalisation; lossy every time is a leak of a
			// different kind — the value would drift on each save.
			again := rewriteHTML(t, rev, rewriteHTML(t, fwd, back, NewStats(false)), NewStats(false))
			if again != back {
				t.Errorf("not a fixed point — the value drifts on every round trip:"+
					"\n once  %s\n twice %s", back, again)
			}
		})
	}
}

// How much of the corpus actually exercises the splice, asserted rather than
// assumed.
//
// "identity map byte-identical over 52 files, 5,945,950 bytes" reads as six
// megabytes of coverage. Under the real map 35 of 51 files were byte-identical:
// 69% of those bytes contain no canonical origin at all, the largest page in the
// corpus (925 KB) has none, and two thirds of the *adversarial* fixtures assert
// only that the tokenizer round-trips bytes. The splice was exercised over
// roughly 1.8 MB, not 5.9.
//
// That is not wrong to have — real pages that contain no origin are a fair
// sample — but it must not be silently diluted, and the number should be
// visible next to the one that overstates it.
func TestCorpusExercisesTheSplice(t *testing.T) {
	fwd := pairMatcher(t, "https://www.acmecorp.fi", "https://wt-a--acmecorp.ddev.site")

	var files, exercised, exercisedBytes, totalBytes int
	var advExercised, advTotal int
	for _, f := range corpusFiles(t) {
		in, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		files++
		totalBytes += len(in)
		adv := strings.Contains(f, "adv")
		if adv {
			advTotal++
		}
		if !bytes.Equal(in, runHTML(t, in, fwd, Options{})) {
			exercised++
			exercisedBytes += len(in)
			if adv {
				advExercised++
			}
		}
	}
	t.Logf("the splice runs on %d of %d files, %d of %d bytes (%.0f%%); "+
		"%d of %d adversarial fixtures contain an origin",
		exercised, files, exercisedBytes, totalBytes,
		100*float64(exercisedBytes)/float64(totalBytes), advExercised, advTotal)

	// Floors, not targets: they exist so that adding fixtures that contain no
	// origin cannot quietly lower the real coverage while the file count climbs.
	if exercised < 17 {
		t.Errorf("only %d files exercise the splice, was 17", exercised)
	}
	if advExercised < 13 {
		t.Errorf("only %d adversarial fixtures contain an origin, was 13", advExercised)
	}
}

// Matcher.Validate promises the map is a fixed point — that no variant origin
// is matched by a canonical one, so nothing double-rewrites. It probes that with
// three encodings through the byte matcher.
//
// It cannot do better: Validate lives in internal/origin, which this package
// imports, so it structurally cannot run the locator, the fold or any of the
// four views it now guards. The promise is therefore wider than the check, and
// the difference is exactly the surface every round since nine has found a bug
// in. So the property is asserted here instead, where the whole pipeline is
// reachable, over the spellings the views decode.
func TestValidatedMapsAreFixedPointsThroughEveryView(t *testing.T) {
	for _, c := range []struct{ name, canonical, variant string }{
		{"the ordinary ddev shape", "https://www.example.fi", "https://wt-a--example.ddev.site"},
		{"a variant with a port", "https://www.example.fi", "http://localhost:8080"},
		{"a variant on the other scheme", "https://www.example.fi", "http://v.ddev.site"},
		{"a canonical with a port", "https://www.example.fi:8443", "https://v.ddev.site"},
		{"an IPv6 variant", "https://www.example.fi", "http://[::1]:8080"},
		// The shape most likely to break the promise: the variant is a label
		// *under* the canonical, so anchoring is the only thing separating them.
		{"a variant under the canonical host", "https://example.fi", "https://wt-a.example.fi"},
	} {
		t.Run(c.name, func(t *testing.T) {
			canon := origin.MustParse(c.canonical)
			variant := origin.MustParse(c.variant)
			m, err := origin.NewMatcher([]origin.Pair{{Canonical: canon, Variant: variant}})
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Validate(); err != nil {
				t.Skipf("Validate refuses this map, so it makes no promise: %v", err)
			}

			host := variant.HostPort()
			// Every spelling a view decodes, applied to the *variant* — which
			// Validate says nothing may match.
			for _, sp := range []struct{ name, in string }{
				{"plain", `<a href="` + variant.String() + `/x">t</a>`},
				{"json-escaped", `<a href="` + variant.Scheme + `:\/\/` + host + `/x">t</a>`},
				{"backslashes", `<a href="` + variant.Scheme + `:\\` + host + `/x">t</a>`},
				{"a long slash run", `<a href="` + variant.Scheme + `:///` + host + `/x">t</a>`},
				{"uppercase", `<a href="` + variant.Scheme + `://` + strings.ToUpper(host) + `/x">t</a>`},
				{"css-escaped", `<div style="background:url(` + variant.Scheme + `\3a \2f \2f ` + host + `/x.png)">t</div>`},
				{"reference-encoded", `<a href="` + variant.Scheme + `:&#47;&#47;` + host + `/x">t</a>`},
				{"refs spelling css", `<div style="background:url(` + variant.Scheme + `&#92;3a &#92;2f &#92;2f ` + host + `/x.png)">t</div>`},
				{"in a text node", `<p>see ` + variant.String() + `/x</p>`},
			} {
				t.Run(sp.name, func(t *testing.T) {
					out := rewriteHTML(t, m, sp.in, NewStats(false))
					if out != sp.in {
						t.Errorf("Validate called this map a fixed point, but a "+
							"variant origin was rewritten — a second pass would "+
							"double-rewrite:\n in  %s\n out %s", sp.in, out)
					}
				})
			}
		})
	}
}

// --explain has to point at the byte that leaked, and every view has to agree
// about where that is.
//
// w.record added `base` to each event's offset and then handed the events to
// Stats.Record, which adds `base` itself — so all five newer views reported
// every event at twice the value's offset in the document, while the byte
// matcher, which goes through Record alone, stayed correct. A real page showed
// a mixture of right and wrong offsets, which is worse than uniformly wrong
// because it looks credible. §5.8 makes this the tool you point at a leak.
//
// The padding exists so the value's own base is large enough that a doubled
// offset cannot coincide with the right one.
func TestExplainOffsetsPointAtTheHost(t *testing.T) {
	m := viewMap(t)
	pad := strings.Repeat("Z", 40)

	for _, c := range []struct{ name, in string }{
		{"the css view in a style attribute",
			`<div style="` + pad + `background:url(https\3a \2f \2f www.example.fi/x)">t</div>`},
		{"the composed view in a style attribute",
			`<div style="` + pad + `background:url(https&#92;3a &#92;2f &#92;2f www.example.fi/x)">t</div>`},
		{"the css view in a style element",
			`<style>` + pad + `a{background:url(https\3a \2f \2f www.example.fi/x)}</style>`},
		{"the percent view in a script",
			`<script>` + pad + `f("https%3A%5C%2F%5C%2Fwww.example.fi%2Fx")</script>`},
		{"the reference view in foreign content",
			`<svg><style>` + pad + `a{background:url(https:&#47;&#47;www.example.fi/x)}</style></svg>`},
		// The byte matcher, which was always correct — it is here so the test
		// fails if a "fix" breaks the path that never had the bug.
		{"the byte matcher in an attribute",
			`<a href="https://www.example.fi/x" data-x="` + pad + `">t</a>`},
		{"the byte matcher in prose",
			`<p>` + pad + ` see https://www.example.fi/x</p>`},
		// The two views that build their events inline rather than through
		// w.record. d3ad6a7's message said it had fixed "all five newer views";
		// these were missed, and they are the ones whose own comments say a
		// non-zero count is worth looking at — so the surfaces that send a
		// developer to --explain were the ones reporting an offset past the end
		// of the document.
		{"the obfuscated-separator view",
			`<p>` + pad + ` see https:\\www.example.fi/x</p>`},
		{"the fold, in an attribute",
			`<a href="https://WWW.EXAMPLE.FI/x" data-x="` + pad + `">t</a>`},
		// A literal soft hyphen, which is what the fold is for — the reference
		// spelling is not decoded in a text node.
		{"the fold, in prose",
			"<p>" + pad + " see https://www.exam\u00adple.fi/x</p>"},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := NewStats(true)
			if out := rewriteHTML(t, m, c.in, st); out == c.in {
				t.Fatalf("nothing was rewritten, so this asserts nothing:\n%s", out)
			}
			ev := st.Events()
			if len(ev) == 0 {
				t.Fatal("no events recorded")
			}
			// The offset must point at the text the event names. The byte
			// matcher names the whole origin and the views name the host, so
			// comparing against a fixed index would assert a convention rather
			// than a property — this holds for both.
			for _, e := range ev {
				if e.Action != origin.ActionRewrote || e.Text == "" {
					continue
				}
				end := e.Offset + len(e.Text)
				if e.Offset < 0 || end > len(c.in) || c.in[e.Offset:end] != e.Text {
					got := "out of range"
					if e.Offset >= 0 && end <= len(c.in) {
						got = c.in[e.Offset:end]
					}
					t.Errorf("%s says %q is at offset %d, where the document has %q"+
						" (the host is at %d)",
						e.Surface, e.Text, e.Offset, got,
						strings.Index(c.in, e.Text))
				}
			}
		})
	}
}

// The census counts splices, not values — on every path.
//
// The reference view in an attribute was the last one still reporting a single
// synthetic event per value, with no text: a `srcset` holding three
// reference-encoded origins plus a fusing fragment counted 1 where every other
// view counts 3, and --explain pointed at the start of the value rather than at
// any origin. Same class as the bug spliceHostsLog was added to fix, one path
// left behind.
//
// The fusing fragment is what forces this path: decodeURLRefs declines the
// whole value when any fragment in it would fuse into a new reference, so the
// other two paths that cover an attribute both bow out and only the view runs.
func TestTheCensusCountsSplicesNotValues(t *testing.T) {
	m := viewMap(t)
	in := `<img srcset="https:&#47;&#47;www.example.fi/a.png 1x, ` +
		`https:&#47;&#47;www.example.fi/b.png 2x, ` +
		`https:&#47;&#47;www.example.fi/c.png 3x?q=&#6&#48;;">`

	st := NewStats(true)
	out := rewriteHTML(t, m, in, st)
	if strings.Contains(out, "www.example.fi") {
		t.Fatalf("a canonical origin survives:\n%s", out)
	}
	if got := st.Total(); got != 3 {
		t.Errorf("the census counted %d splices, want 3:\n%s", got, out)
	}
	for _, e := range st.Events() {
		if e.Action == origin.ActionRewrote && e.Text == "" {
			t.Errorf("%s recorded an event naming no bytes, so --explain has "+
				"nothing to point at", e.Surface)
		}
	}
}

// A character reference spelling TAB, LF or CR *inside* an origin, on the
// surfaces where the consumer decodes references.
//
// stripForRefs deliberately leaves these alone — parseURLRef must never emit a
// control character, which was one of the XSS holes this file sits next to.
// stripForCSS falls through to stripForURL when there is no backslash, and
// stripForURL is what removes them. So composeView(stripForRefs, stripForCSS)
// is the engine's only refs-then-URL-strip view, and an allocation
// optimisation that skipped it "when the references do not spell a backslash"
// turned every shape here into a byte-identical pass-through with the census
// reporting a clean page.
//
// Chrome preserves a reference to LF through XML attribute-value normalisation
// and ada then strips it: `new URL("https:/\n/www.example.fi/x")` resolves to
// host www.example.fi. Under production-canonical that is a live fetch to the
// client's site carrying the developer's session — test 28.
func TestRemovableReferencesInsideAnOrigin(t *testing.T) {
	m := viewMap(t)
	for _, sp := range []string{
		`https:&#47;&#10;&#47;www.example.fi/x`,
		`https:&#47;&#9;&#47;www.example.fi/x`,
		`https:&#47;&#13;&#47;www.example.fi/x`,
		`https:&#47;&#47;www.exam&#9;ple.fi/x`,
		`&#47;&#9;&#47;www.example.fi/x`,
		`https:&#47;&Tab;&#47;www.example.fi/x`,
		`https:&#47;&NewLine;&#47;www.example.fi/x`,
	} {
		// The two dimensions this test did not vary, and the two the next
		// round's leaks lived in: *which surface*, and whether an unrelated
		// backslash appears anywhere else in the buffer.
		//
		// The composed view was wired to style surfaces alone, so a <script> or
		// a text node inside <svg>/<math> was uncovered — and Chrome decodes
		// references in both. And each decoder's removal pass tested
		// isURLStripped alone, reaching the reference-aware pass only as a
		// fall-through when its own trigger byte was absent; so one `\` anywhere
		// in the body — a Windows path, a CSS escape, a regex — disarmed it for
		// every origin in that body.
		for _, noise := range []struct{ name, extra string }{
			{"alone", ""},
			{"with a backslash elsewhere in the buffer", `<desc>C:\tmp</desc>`},
		} {
			t.Run(sp+" "+noise.name, func(t *testing.T) {
				// A standalone SVG or XML body, which has no HTML parser above it.
				svg := `<svg xmlns="http://www.w3.org/2000/svg">` + noise.extra +
					`<image href="` + sp + `"/></svg>`
				if out := string(HostLeaksXML(m, []byte(svg), false)); strings.Contains(out, "www.example.fi") {
					t.Errorf("standalone XML: a canonical origin survives:\n%s", out)
				}
				// And every surface inside foreign content, where the HTML
				// tokenizer never enters a raw-text state so references decode.
				for _, shape := range []string{
					`<svg>` + noise.extra + `<style>a{background:url(` + sp + `)}</style></svg>`,
					`<svg>` + noise.extra + `<script>fetch("` + sp + `")</script></svg>`,
					`<svg>` + noise.extra + `<text>` + sp + `</text></svg>`,
				} {
					if out := rewriteHTML(t, m, shape, NewStats(false)); strings.Contains(out, "www.example.fi") {
						t.Errorf("a canonical origin survives:\n in  %s\n out %s", shape, out)
					}
				}
				// The control: an HTML <script> is raw text, so its references
				// are not decoded and Chrome does not resolve this. Rewriting it
				// would be the mirror error.
				plain := `<script>y("` + sp + `")</script>`
				if out := rewriteHTML(t, m, plain, NewStats(false)); out != plain {
					t.Errorf("an HTML script decodes no references, but it was rewritten:\n%s", out)
				}
			})
		}
	}
}

// The two paths must agree about what is a path segment and what is an
// authority.
//
// foldedHostLeak anchors its slash-run candidates on a token boundary, and its
// comment cites `https://cdn.other/p//www.example.fi/q` as the reason. The
// standalone path ran the same scan with no anchor, so the HTML pipeline left
// that path segment alone and HostLeaks rewrote it — and the gate is one
// non-ASCII byte *anywhere in the buffer*, so a single `ä` in a Finnish feed
// armed it for every candidate in the body. In the request direction that edits
// a path on its way into the shared database.
func TestBothPathsAgreeOnWhatIsAnAuthority(t *testing.T) {
	m := viewMap(t)
	for _, c := range []struct {
		name, in string
		change   bool
	}{
		{"a path segment that looks like a scheme-relative authority",
			"https://cdn.other/p//www.example.fi/q ä", false},
		{"a real scheme-relative reference", "//www.example.fi/q ä", true},
		{"one at the start of a value after a space", "see //www.example.fi/q ä", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			flat := string(HostLeaks(m, []byte(c.in), false))
			html := rewriteHTML(t, m, "<p>"+c.in+"</p>", NewStats(false))
			htmlInner := strings.TrimSuffix(strings.TrimPrefix(html, "<p>"), "</p>")
			if flat != htmlInner {
				t.Errorf("the two paths disagree:\n flat %s\n html %s", flat, htmlInner)
			}
			if got := flat != c.in; got != c.change {
				t.Errorf("rewrote=%v, want %v:\n in  %s\n out %s", got, c.change, c.in, flat)
			}
		})
	}
}

// slashRunStarts must stay a subset of urlTokenStarts.
//
// Anchoring slashRunStarts on a token boundary — which it needed, to stop
// mistaking a path segment for an authority — made its second pass over the
// same view unable to find anything the first had not, so rewriteAll dropped
// it. That saved a full extra pass on every buffer containing one non-ASCII
// byte: on an 8 MiB body a single `ä` was costing 136 MB of transient
// allocation for a pass that could no longer match.
//
// This is the property that made the removal safe, so it is asserted rather
// than assumed. If either scan changes, this fails before a leak appears.
func TestSlashRunStartsAreASubsetOfTokenStarts(t *testing.T) {
	alphabet := []byte(`/\:hs.aä[]& `)
	var buf []byte
	var rec func(depth int)
	rec = func(depth int) {
		if depth == 0 {
			tok := map[int]bool{}
			for _, i := range urlTokenStarts(buf) {
				tok[i] = true
			}
			for _, i := range slashRunStarts(buf) {
				if !tok[i] {
					t.Fatalf("slashRunStarts(%q) yields %d, which urlTokenStarts does not",
						buf, i)
				}
			}
			return
		}
		for _, c := range alphabet {
			buf = append(buf, c)
			rec(depth - 1)
			buf = buf[:len(buf)-1]
		}
	}
	// Every string of length 5 over the alphabet that matters: the slashes and
	// backslashes the scan looks for, a scheme, a host character, a non-ASCII
	// byte to arm the old gate, the brackets that made hostRange quadratic, an
	// ampersand, and a space to make a boundary.
	rec(5)
}

// urlLeaks must not return after the locator and skip the reference view.
//
// The comment above that call records that the *entity* pass's early return
// there was a leak and was removed; this one was left. It skipped refsLeak —
// "the view that survives a value decodeURLRefs declines" — so an attribute
// holding a fusing fragment, an origin the locator catches, and a
// reference-encoded origin rewrote the first and served the second live, with
// the census reporting a successful rewrite and zero skips.
//
// The fusing fragment is what forces the path: `&#6`+`&#48;`+`;` makes
// decodeURLRefs decline the whole value, so only the view can see the second
// origin. Chrome decodes `&#47;&#47;` in srcset and ping, and POSTs to a ping
// URL on click.
func TestTheLocatorDoesNotShortCircuitTheReferenceView(t *testing.T) {
	m := viewMap(t)
	for _, c := range []struct{ name, in string }{
		{"srcset", `<img srcset="https:\\www.example.fi/1.png 1x, &#6&#48;; ` +
			`https:&#47;&#47;www.example.fi/2.png 2x">`},
		{"ping", `<a ping="https:\\www.example.fi/1 &#6&#48;; ` +
			`https:&#47;&#47;www.example.fi/2" href="/x">t</a>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := NewStats(false)
			out := rewriteHTML(t, m, c.in, st)
			if strings.Contains(out, "www.example.fi") {
				t.Errorf("one origin was rewritten and another left live:\n in  %s\n out %s",
					c.in, out)
			}
			if st.Total() < 2 {
				t.Errorf("the census counted %d, so it reports a clean rewrite on a "+
					"value that had two origins:\n%s", st.Total(), out)
			}
		})
	}
}
