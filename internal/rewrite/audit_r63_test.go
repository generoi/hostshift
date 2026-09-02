package rewrite

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 63, on 4ea600a, auditing round 62's fix to the foreign-content model.
//
// Round 62 replaced round 61's name-match with a namespace-scoped vocabulary
// stack: `w.foreignNS` holds the open `<svg>`/`<math>` elements and
// `integrationPointIn(name, ns)` asks whether a name is an integration point *in
// the vocabulary it appears in*. The direction is right. The scope is not: the
// stack stores the **tag name** as the namespace and pops by count, and neither
// is what HTML 13.2.6.5 says.
//
//   - **Push.** "Any other start tag" in foreign content ends with *"Insert a
//     foreign element for the token, **in the same namespace as the adjusted
//     current node**."* A `<math>` whose parent is an SVG element is an element
//     in the **SVG** namespace — it is not MathML, and its `<mi>` child is not a
//     MathML text integration point. hostshift pushes the literal string
//     `"math"`, so `currentNS()` says MathML and `<mi>` is read as an
//     integration point that resumes HTML rules.
//   - **Pop.** "Any other end tag" walks the stack and, on a name mismatch,
//     falls through to the enclosing *HTML* element and processes the token
//     under HTML rules — where a stray `</math>` is simply ignored. hostshift
//     pops the top entry whatever its name is.
//
// Both errors resolve to *"HTML rules apply"* while the parser is still in
// foreign content, which is the direction that **withholds** the reference
// decode — the leak sign, not the over-decode sign §4.4 chooses on purpose.
// Every leaking shape a 57,348-document walk over the foreign-content token
// alphabet found interleaves `svg` and `math`; none is reachable with one
// vocabulary alone. That is this defect's fingerprint and no other.
//
// The oracle is golang.org/x/net/html's *parser*, as the previous three rounds
// used it, and it agrees with the spec on both points. Its tree for the first
// fixture is
//
//	svg:svg > svg:math > svg:mi > svg:script > #text `fetch("https://www.example.fi/x")`
//
// — every element in the SVG namespace, the references decoded, and the decoded
// text resolving to `https://www.example.fi/x`, this map's canonical. That is
// test 28: a dereferenceable production origin in the browser, inside a
// `<script>` the page runs, against a live site the developer is logged into.

const r63Ref = `https:&#47;&#47;www.example.fi/x`

// The namespace of a nested foreign element is its parent's, not its own name.
//
// `<svg><math>` is an SVG element called "math". Round 62 records the vocabulary
// from the tag name, so it reads the subtree as MathML and treats `<mi>`, `<mo>`,
// `<mn>`, `<ms>`, `<mtext>` and `<annotation-xml encoding=text/html>` as
// integration points there. They are not: they are ordinary SVG elements the
// parser stays foreign inside, and the browser decodes character references in
// the `<script>` and `<style>` below them.
//
// The mirror holds: `<math><svg>` is a MathML element called "svg", so its
// `desc`/`title`/`foreignObject` children are not HTML integration points
// either.
func TestR63NestedForeignInheritsTheParentNamespace(t *testing.T) {
	m := obfMatcher(t)
	for _, c := range []struct{ name, doc string }{
		{"svg > math > mi > script",
			`<svg><math><mi><script>fetch("` + r63Ref + `")</script></mi></math></svg>`},
		{"svg > math > mtext > style",
			`<svg><math><mtext><style>@import url("` + r63Ref + `");</style></mtext></math></svg>`},
		{"svg > math > annotation-xml[text/html] > script",
			`<svg><math><annotation-xml encoding="text/html"><script>fetch("` + r63Ref + `")</script></annotation-xml></math></svg>`},
		{"math > svg > desc > script",
			`<math><svg><desc><script>fetch("` + r63Ref + `")</script></desc></svg></math>`},
		{"math > svg > foreignObject > style",
			`<math><svg><foreignObject><style>@import url("` + r63Ref + `");</style></foreignObject></svg></math>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !parserDecodesReferences(t, c.doc) {
				t.Fatalf("oracle: the parser does not decode here, so this fixture "+
					"no longer tests the claim:\n  %s", c.doc)
			}
			out := rewriteHTML(t, m, c.doc, nil)
			if !strings.Contains(out, "wt-a--example.ddev.site") {
				t.Errorf("the parser stays in foreign content here and decodes the\n"+
					"  references to https://www.example.fi/x — this map's canonical —\n"+
					"  but hostshift read the nested element's namespace from its own\n"+
					"  tag name, called the child an integration point, and withheld the\n"+
					"  decode. The canonical origin went to the browser unrewritten.\n"+
					"  in:  %s\n  out: %s", c.doc, out)
			}
		})
	}
}

// A mismatched foreign end tag closes nothing.
//
// `w.foreignNS` is popped on any `</svg>` or `</math>` while it is non-empty,
// without checking that the name matches the top of the stack. HTML 13.2.6.5
// "any other end tag" walks down instead: with `svg:svg` current and a `</math>`
// token, the walk reaches `body` — an HTML element — and reprocesses the token
// under HTML rules, where it matches nothing and is dropped. The `<svg>` stays
// open and the parser stays foreign.
//
// One stray tag therefore disarms the reference view for the rest of the
// element, which is the same failure mode round 61 fixed for `foreignObject`'s
// counter, still present one level up in the stack that replaced it.
func TestR63MismatchedForeignEndTagPopsTheWrongElement(t *testing.T) {
	m := obfMatcher(t)
	for _, c := range []struct{ name, doc string }{
		{"svg, stray </math>, script",
			`<svg></math><script>fetch("` + r63Ref + `")</script></svg>`},
		{"svg, stray </math>, style",
			`<svg></math><style>@import url("` + r63Ref + `");</style></svg>`},
		{"math, stray </svg>, style",
			`<math></svg><style>@import url("` + r63Ref + `");</style></math>`},
		{"svg > g, stray </math>, script",
			`<svg><g></math><script>fetch("` + r63Ref + `")</script></g></svg>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !parserDecodesReferences(t, c.doc) {
				t.Fatalf("oracle: the parser does not decode here, so this fixture "+
					"no longer tests the claim:\n  %s", c.doc)
			}
			out := rewriteHTML(t, m, c.doc, nil)
			if !strings.Contains(out, "wt-a--example.ddev.site") {
				t.Errorf("a stray end tag for the *other* vocabulary popped the open\n"+
					"  element the parser never closed, so hostshift left foreign content\n"+
					"  while the browser stayed in it and decoded the references to\n"+
					"  https://www.example.fi/x. Test 28, from one unbalanced tag.\n"+
					"  in:  %s\n  out: %s", c.doc, out)
			}
		})
	}
}

// A mismatched foreign end tag pops nothing *unless* something below it matches.
//
// The fix for the previous test is "a mismatch pops nothing", and that on its own
// is wrong in the other direction: 13.2.6.5 walks *down* the stack, so a match
// further out does close everything above it. `<math><svg></math>` closes both —
// the oracle puts the `<script>` after it in the HTML namespace, outside the
// math — and an HTML `<script>` is one the parser does *not* decode references
// in. A model that only ever checked the innermost element would stay foreign
// for the rest of the document and decode a `&#47;&#47;` the browser reads
// literally, rewriting bytes that are not a URL to anything.
//
// This is the over-decode direction §4.4 chooses when it must choose, so it is
// not test 28. It is here because it is the only case that distinguishes the
// walk from a check of the innermost element, and without it the walk is
// untested scope.
func TestR63AMatchDeeperInTheStackClosesEverythingAbove(t *testing.T) {
	m := obfMatcher(t)
	for _, c := range []struct{ name, doc string }{
		{"math > svg, stray </math>, script",
			`<math><svg></math><script>fetch("` + r63Ref + `")</script>`},
		{"svg > math, stray </svg>, script",
			`<svg><math></svg><script>fetch("` + r63Ref + `")</script>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if parserDecodesReferences(t, c.doc) {
				t.Fatalf("oracle: the parser decodes here after all, so this fixture "+
					"no longer tests the claim:\n  %s", c.doc)
			}
			out := rewriteHTML(t, m, c.doc, nil)
			if out != c.doc {
				t.Errorf("the end tag closes both vocabularies — the oracle puts this\n"+
					"  <script> in the HTML namespace — so the references in it are\n"+
					"  bytes a reader sees and not a URL the browser resolves. Rewriting\n"+
					"  them changes a page that had nothing to rewrite.\n"+
					"  in:  %s\n  out: %s", c.doc, out)
			}
		})
	}
}

// The CSS view belongs to the `<style>`, not to the token that contains it.
//
// `<title>` inside `<svg>` is an HTML integration point, so the parser builds a
// real `<style>` in it and runs its CSS tokenizer — but the tokenizer here hands
// the whole element back as one raw-text token. Round 62 switched the entire
// token to `SurfaceInlineStyle` whenever it contained `<style` anywhere, so a
// `\3a \2f \2f` run in the *text* beside the stylesheet was unescaped and
// rewritten. That text is bytes a reader sees; the escapes are not syntax there
// and no browser resolves them. It is the text/plain over-rewrite round 60 fixed,
// returning in one element.
//
// The character-reference decode is unaffected and must stay: the text of a
// foreign `<title>` really is a place the parser decodes.
func TestR63TheCSSViewStopsAtTheStyleElement(t *testing.T) {
	m := obfMatcher(t)
	const esc = `https\3a \2f \2f www.example.fi/x`
	for _, c := range []struct{ name, doc, want string }{
		{"escapes after the stylesheet are text",
			`<svg><title><style>a{b:c}</style> ` + esc + `</title></svg>`,
			`<svg><title><style>a{b:c}</style> ` + esc + `</title></svg>`},
		{"escapes before the stylesheet are text",
			`<svg><title>` + esc + ` <style>a{b:c}</style></title></svg>`,
			`<svg><title>` + esc + ` <style>a{b:c}</style></title></svg>`},
		{"escapes inside the stylesheet are still CSS",
			`<svg><title><style>@import url(` + esc + `);</style> ok</title></svg>`,
			`<svg><title><style>@import url(https\3a \2f \2f wt-a--example.ddev.site/x);</style> ok</title></svg>`},
		{"an unclosed stylesheet runs to the end of the token",
			`<svg><title><style>@import url(` + esc + `);</title></svg>`,
			`<svg><title><style>@import url(https\3a \2f \2f wt-a--example.ddev.site/x);</title></svg>`},
		{"references in the text are still decoded",
			`<svg><title>ref ` + r63Ref + ` <style>a{b:c}</style></title></svg>`,
			`<svg><title>ref https:&#47;&#47;wt-a--example.ddev.site/x <style>a{b:c}</style></title></svg>`},
		{"references *after* the stylesheet are still decoded",
			`<svg><title><style>a{b:c}</style> ref ` + r63Ref + `</title></svg>`,
			`<svg><title><style>a{b:c}</style> ref https:&#47;&#47;wt-a--example.ddev.site/x</title></svg>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if out := rewriteHTML(t, m, c.doc, nil); out != c.want {
				t.Errorf("the CSS view ran over bytes that are not CSS\n"+
					"  in:   %s\n  out:  %s\n  want: %s", c.doc, out, c.want)
			}
		})
	}
}

// A reference-encoded origin in body prose is one a reader copy-pastes.
//
// An HTML parser decodes character references in body text — that is not
// conditional on anything — so `<p>https:&#47;&#47;canonical/x</p>` renders as a
// live production URL. §4.4 opens with exactly this hazard, and the M6 pilot
// found it in the wild as a privacy-policy paragraph quoting its own URL.
//
// The gate fired only in foreign content, so hostshift rewrote these bytes
// inside `<title>` and shipped them inside `<p>`. Not test 28 — the browser does
// not dereference it — which is why it took until round 63 to notice.
func TestR63ReferencesInBodyProseAreDecoded(t *testing.T) {
	m := obfMatcher(t)
	const want = `https:&#47;&#47;wt-a--example.ddev.site/x`
	for _, c := range []struct{ name, doc, want string }{
		{"a paragraph", `<p>See ` + r63Ref + ` for more</p>`, `<p>See ` + want + ` for more</p>`},
		{"a list item", `<li>` + r63Ref + `</li>`, `<li>` + want + `</li>`},
		{"still decoded in a title", `<title>` + r63Ref + `</title>`, `<title>` + want + `</title>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !parserDecodesReferences(t, c.doc) {
				t.Fatalf("oracle: the parser does not decode here, so this fixture "+
					"no longer tests the claim:\n  %s", c.doc)
			}
			if out := rewriteHTML(t, m, c.doc, nil); out != c.want {
				t.Errorf("the rendered text is a production URL a developer copies\n"+
					"  in:   %s\n  out:  %s\n  want: %s", c.doc, out, c.want)
			}
		})
	}
}

// A port is a number, and hostshift compared it as a string.
//
// ada resolves `https://www.example.fi:0443/x` to `https://www.example.fi/x`:
// the port is parsed as an integer, leading zeros and all, and there is no bound
// on how many of them there may be. `NormalisePort` compared the digits to
// "443" and "80" as strings, so every padded spelling of a default port missed
// the map and a dereferenceable production origin went to the browser
// unrewritten. Test 28, and reachable from any CMS that renders a port.
//
// Round 63's breaker flagged the unbounded digit run as unsettled and could not
// construct a leak from the match *length*; the leak was in what the digits were
// compared against.
func TestR63APaddedPortIsTheSamePort(t *testing.T) {
	m := obfMatcher(t)
	const v = `<a href="https://wt-a--example.ddev.site/x">a</a>`
	for _, c := range []struct{ name, doc, want string }{
		{"one leading zero", `<a href="https://www.example.fi:0443/x">a</a>`, v},
		{"many leading zeros", `<a href="https://www.example.fi:0000000443/x">a</a>`, v},
		{"the bare default port still works", `<a href="https://www.example.fi:443/x">a</a>`, v},
		// The other direction: a padded port that is *not* this origin's stays put.
		{"a padded non-default port is a different origin",
			`<a href="https://www.example.fi:08443/x">a</a>`,
			`<a href="https://www.example.fi:08443/x">a</a>`},
		{"all zeros is the port 0, not the default",
			`<a href="https://www.example.fi:0/x">a</a>`,
			`<a href="https://www.example.fi:0/x">a</a>`},
		{"a port no parser accepts is not a URL",
			`<a href="https://www.example.fi:99999999999/x">a</a>`,
			`<a href="https://www.example.fi:99999999999/x">a</a>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if out := rewriteHTML(t, m, c.doc, nil); out != c.want {
				t.Errorf("padded ports resolve to the same origin the map names\n"+
					"  in:   %s\n  out:  %s\n  want: %s", c.doc, out, c.want)
			}
		})
	}
}

// A form encodes what it was given, and the response direction gave it a
// percent-encoded origin.
//
// `https%3A%2F%2Fh` is one of the three spellings §4.4 requires, so the response
// direction rewrites it to the variant inside an attribute. The browser posts
// that value back with the `%` itself encoded — `https%253A%252F%252Fh` — and no
// spelling matched, so the request direction could not take it back. A *variant*
// hostname reached the app and went into the shared database, which is §4.3 and
// has no undo. Round 63's daily audit read one back out of a real database.
//
// The serialized length is the second half: repaired on the way out, so a value
// that comes home with the host mapped back and the length left stale is a
// `s:32:` over 26 bytes, and PHP's unserialize() returns false for the whole
// option.
func TestR63AFormEncodedOriginComesHome(t *testing.T) {
	rev, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://wt-a--example.ddev.site"),
		Variant:   origin.MustParse("https://www.example.fi"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	rw := func(b []byte) []byte {
		out, _ := rev.Rewrite(b, SurfaceRequestBody, false)
		return out
	}
	const variant = "https://wt-a--example.ddev.site/x" // 33 bytes
	const canon = "https://www.example.fi/x"            // 24
	blob := func(u string) string {
		return `a:1:{s:1:"k";s:` + strconv.Itoa(len(u)) + `:"` + u + `";}`
	}
	esc := url.QueryEscape
	for _, c := range []struct{ name, in, want string }{
		{"a bare origin under two layers",
			"v=" + esc(esc(variant)), "v=" + esc(esc(canon))},
		{"a serialized blob under two layers, length repaired",
			"v=" + esc(esc(blob(variant))), "v=" + esc(esc(blob(canon)))},
		{"one layer still works",
			"v=" + esc(blob(variant)), "v=" + esc(blob(canon))},
		{"no encoding still works",
			"v=" + blob(variant), "v=" + blob(canon)},
		{"the peeled field is found among others",
			"a=1&b=hello+world&c=" + esc(esc(variant)),
			"a=1&b=hello+world&c=" + esc(esc(canon))},
		// The guard: a value whose encoding this cannot reproduce byte for byte
		// is left alone rather than reshaped in passing.
		{"a value that does not round-trip is untouched",
			"v=no%20plus%20style", "v=no%20plus%20style"},
		// The guard is what keeps the peel from reshaping a body in passing:
		// this value *does* carry an origin under two layers, but its spaces are
		// `%20` where re-encoding writes `+`. Rewriting it would hand the app a
		// value whose other bytes had been rewritten too, so it is declined
		// whole. A conservative miss, and a deliberate one.
		{"an origin whose encoding cannot be reproduced is declined, not reshaped",
			"v=x%20y%20" + esc(esc(variant)), "v=x%20y%20" + esc(esc(variant))},
		{"a field with nothing to do is untouched",
			"v=nothing-to-do", "v=nothing-to-do"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := string(RepairSerializedFields([]byte(c.in), rw)); got != c.want {
				t.Errorf("a variant hostname reaches the shared database\n"+
					"  in:   %s\n  got:  %s\n  want: %s", c.in, got, c.want)
			}
		})
	}
}

// An HTML tag from the breakout list ends foreign content where it stands.
//
// 13.2.6.5 lists 44 HTML start tags that pop the parser out of `<svg>`/`<math>`
// — `<p>`, `<div>`, `<br>`, `<table>` and the rest — plus `<font>`, but only
// with a color, face or size attribute. An unclosed `<svg>` followed by ordinary
// markup is what a malformed inline icon looks like, and the model stayed
// foreign for the rest of the document, decoding references in `<script>` and
// `<style>` elements the browser reads as HTML.
//
// Over-decode, so no canonical origin ever shipped from this — it is here
// because it rewrote the value of a JavaScript string that was never a URL.
// The near misses matter as much as the list: `section`, `article` and any SVG
// element are *not* on it, and treating them as breakouts would withhold a
// decode the browser performs, which is test 28.
func TestR63TheBreakoutListEndsForeignContent(t *testing.T) {
	m := obfMatcher(t)
	const ref = r63Ref
	const done = `https:&#47;&#47;wt-a--example.ddev.site/x`
	for _, c := range []struct {
		name, doc string
		foreign   bool
	}{
		{"<p> breaks out", `<svg><p>t</p><div>x</div><script>a("` + ref + `")</script>`, false},
		{"<br> breaks out", `<svg><br><script>a("` + ref + `")</script>`, false},
		{"<table> breaks out", `<svg><table><script>a("` + ref + `")</script>`, false},
		{"<font> with an attribute breaks out",
			`<svg><font color=red><script>a("` + ref + `")</script>`, false},
		{"a bare <font> does not", `<svg><font><script>a("` + ref + `")</script>`, true},
		{"<section> is not on the list", `<svg><section><script>a("` + ref + `")</script>`, true},
		{"an SVG child is not", `<svg><g><script>a("` + ref + `")</script></g></svg>`, true},
		{"the breakout does not reach past an integration point",
			`<svg><foreignObject><div><svg><script>a("` + ref + `")</script>`, true},
		// The breakout pops back to the integration point and stops there, so
		// the <svg> after it is foreign again. Emptying the stack instead looks
		// identical until something re-enters: then the model is one level too
		// shallow and withholds a decode the browser performs.
		{"an <svg> after a breakout inside an integration point is foreign again",
			`<svg><foreignObject><svg><p>x<svg><script>a("` + ref + `")</script>`, true},
		{"and without that <svg> it is HTML",
			`<svg><foreignObject><svg><p>x<script>a("` + ref + `")</script>`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := parserDecodesReferences(t, c.doc); got != c.foreign {
				t.Fatalf("oracle: the parser %s decode here, so this fixture no "+
					"longer tests the claim:\n  %s",
					map[bool]string{true: "does", false: "does not"}[got], c.doc)
			}
			out := rewriteHTML(t, m, c.doc, nil)
			if c.foreign && !strings.Contains(out, done) {
				t.Errorf("the parser is in foreign content and decodes these "+
					"references, but hostshift withheld the decode\n  in:  %s\n  out: %s",
					c.doc, out)
			}
			if !c.foreign && out != c.doc {
				t.Errorf("the parser left foreign content here, so these bytes are "+
					"a string no browser resolves — rewriting them changes a value "+
					"that was never a URL\n  in:  %s\n  out: %s", c.doc, out)
			}
		})
	}
}
