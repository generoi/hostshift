package rewrite

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// Round 61, on 33cb2a6, auditing the surface round 60 opened and did not
// enumerate: the *foreign-content state machine* — the model of which insertion
// mode the browser's parser is in — crossed with the decoders that consult it.
//
// Round 60 added `w.foreignObject`, a second counter beside `w.foreign`, so that
// `<foreignObject>` — "named for handing the parser back to HTML" — stops
// character references being decoded inside it. That is one cell of a grid with
// two axes the change did not draw:
//
//   - HTML has *seven* integration points, not one. SVG's `foreignObject`,
//     `desc` and `title` are HTML integration points; MathML's `mi`, `mo`, `mn`,
//     `ms` and `mtext` are text integration points, and `annotation-xml` is an
//     HTML integration point when its `encoding` is `text/html` or
//     `application/xhtml+xml`. Only the first is modelled.
//   - An integration point is a *re-entrant* boundary. Inside one the parser is
//     back in HTML, and an `<svg>` there puts it back in foreign content. A
//     counter cannot say that; a stack can.
//
// The oracle for "which mode is the parser in" is golang.org/x/net/html's
// *parser* (this project already trusts its tokenizer), which implements HTML
// 13.2.6 including the integration-point rules. The oracle for what the resolved
// bytes then mean is ada, through Node's URL, with the variant origin as base:
//
//	new URL("https://www.example.fi/x", "https://wt-a--example.ddev.site/").host
//	  === "www.example.fi"                      // the decoded form is canonical
//	new URL("https:&#47;&#47;www.example.fi/x", …).host
//	  === "wt-a--example.ddev.site"             // the raw form points nowhere
//
// which is test 28 in both directions at once: where the parser decodes,
// hostshift must rewrite or a production origin reaches the browser; where it
// does not, hostshift must not touch, because nothing there points at production
// and changing it corrupts bytes the reader sees literally.

// parserDecodesReferences reports whether a spec parser decodes character
// references at the position `marker` occupies in doc — the oracle these tests
// measure against.
func parserDecodesReferences(t *testing.T, doc string) bool {
	t.Helper()
	n, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	var text string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			text += n.Data
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Contains(text, "https://www.example.fi/x")
}

const r61Ref = `https:&#47;&#47;www.example.fi/x`

// A depth counter is not a stack, and an integration point is re-entrant.
//
// `w.foreignObject` is incremented on `<foreignObject>` and decremented on
// `</foreignObject>`, and `inForeign` is `w.foreign > 0 && w.foreignObject == 0`.
// So the moment a document re-enters foreign content *inside* a foreignObject —
// `<svg><foreignObject><div><svg>`, which is how every chart library nests an
// icon inside an HTML label — the model says "HTML rules" while the parser says
// "foreign content", and the reference view is withheld from a `<script>` whose
// references the browser decodes and dereferences.
//
// The harm chain is test 28, and the leak is what round 60 *introduced*: before
// 33cb2a6 the gate was `w.foreign > 0` alone, which is right on this shape. The
// same commit's own words for the mirror of this error — "the mirror of the
// error this file exists to prevent" — apply to it in the direction that leaks
// rather than the one that over-rewrites, and the census reports zero.
func TestR61ForeignObjectIsNotAStack(t *testing.T) {
	m := obfMatcher(t)
	for _, c := range []struct{ name, doc string }{
		{"svg > foreignObject > svg",
			`<svg><foreignObject><svg><script>fetch("` + r61Ref + `")</script></svg></foreignObject></svg>`},
		{"svg > foreignObject > div > svg",
			`<svg><foreignObject><div><svg><script>fetch("` + r61Ref + `")</script></svg></div></foreignObject></svg>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !parserDecodesReferences(t, c.doc) {
				t.Fatalf("oracle: the parser does not decode here, so this fixture "+
					"no longer tests the claim:\n  %s", c.doc)
			}
			out := rewriteHTML(t, m, c.doc, nil)
			if !strings.Contains(out, "wt-a--example.ddev.site") {
				t.Errorf("the parser decodes the references here and resolves\n"+
					"  https://www.example.fi/x — this map's canonical — but hostshift\n"+
					"  served the canonical origin through untouched. That is test 28:\n"+
					"  a dereferenceable production origin in the browser, from a\n"+
					"  <script> the page runs.\n  in:  %s\n  out: %s", c.doc, out)
			}
		})
	}
}

// One unbalanced `<foreignObject>` disarms the rest of the document.
//
// The counter is incremented on a start tag and decremented only on an explicit
// matching end tag, in a *streaming tokenizer that keeps no element stack* — and
// the HTML parser closes elements implicitly all the time. `</svg>` pops an open
// `foreignObject` with it (HTML 13.2.6.5, "any other end tag" in foreign
// content); so does the end of the document. hostshift's counter never comes
// back down, so every subsequent `<svg>`/`<math>` subtree on the page loses its
// reference view.
//
// `w.foreign` has had the same unbalanced shape since it was written, and it is
// harmless there because an unbalanced `<svg>` leaves the model *over*-decoding
// — the direction this project chooses on purpose. Round 60 added a counter with
// the same shape and the opposite sign, so the failure mode inverted from
// over-rewrite to leak, and the blast radius is the remainder of the response.
func TestR61AnUnbalancedForeignObjectDisarmsTheRestOfTheDocument(t *testing.T) {
	m := obfMatcher(t)
	const later = `<svg><script>fetch("` + r61Ref + `")</script></svg>`
	for _, c := range []struct{ name, doc string }{
		{"</svg> closes it implicitly", `<svg><foreignObject><div>hi</div></svg>` + later},
		{"end of document closes it", `<svg><foreignObject><div>hi</div>` + later},
		{"uppercase, unbalanced", `<svg><FOREIGNOBJECT>hi</svg>` + later},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !parserDecodesReferences(t, c.doc) {
				t.Fatalf("oracle: the parser does not decode here, so this fixture "+
					"no longer tests the claim:\n  %s", c.doc)
			}
			out := rewriteHTML(t, m, c.doc, nil)
			if !strings.Contains(out, "wt-a--example.ddev.site") {
				t.Errorf("an unbalanced <foreignObject> earlier in the document left\n"+
					"  the counter armed, so a later <svg><script> whose references the\n"+
					"  parser decodes to https://www.example.fi/x went out untouched —\n"+
					"  test 28, scoped to the whole rest of the response.\n"+
					"  in:  %s\n  out: %s", c.doc, out)
			}
		})
	}
}

// The two decoders that ask "is the parser in foreign content" ask it
// differently, and only one was changed.
//
// rewriteValueInner computes `inForeign := w.foreign > 0 && w.foreignObject == 0`
// for the reference view — and eight lines below, the refs-then-CSS composition
// still gates on the bare `w.foreign > 0`. Inside a `<foreignObject>` an HTML
// `<style>` is RAWTEXT: the parser decodes nothing in it, ada resolves the raw
// bytes to the *variant* base, and the second half of the oracle forbids
// touching them. hostshift rewrites them anyway.
//
// Not a leak — an over-rewrite, of bytes the reader sees literally, which is the
// error 33cb2a6's own comment on the line above calls "the mirror of the error
// this file exists to prevent". It is the same defect round 60 fixed, at the
// second of the two sites that share the question.
func TestR61TheRefsCSSCompositionStillIgnoresForeignObject(t *testing.T) {
	m := obfMatcher(t)
	for _, c := range []struct{ name, doc string }{
		{"references alone",
			`<svg><foreignObject><style>@import url(https:&#47;&#47;www.example.fi/x);</style></foreignObject></svg>`},
		{"references spelling CSS escapes",
			`<svg><foreignObject><style>@import url(https:&#92;2f&#92;2f www.example.fi/x);</style></foreignObject></svg>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if parserDecodesReferences(t, c.doc) {
				t.Fatalf("oracle: the parser *does* decode here, so this fixture no "+
					"longer tests the claim:\n  %s", c.doc)
			}
			out := rewriteHTML(t, m, c.doc, nil)
			if out != c.doc {
				t.Errorf("inside <foreignObject> an HTML <style> is RAWTEXT: the parser\n"+
					"  decodes nothing, so ada resolves these bytes against the variant\n"+
					"  base and nothing here points at production. Rewriting them changes\n"+
					"  a document the reader sees literally.\n  in:  %s\n  out: %s",
					c.doc, out)
			}
		})
	}
}

// Six of the seven integration points are not modelled at all.
//
// `foreignObject` is one of three HTML integration points in SVG — `desc` and
// `title` are the others — and MathML has five text integration points (`mi`,
// `mo`, `mn`, `ms`, `mtext`) plus `annotation-xml` when its `encoding` names
// HTML. In every one of them the parser is back on HTML rules and decodes
// nothing inside a `<script>` or a `<style>`; hostshift decodes in all of them,
// because `w.foreign > 0` is still true.
//
// Same direction as the case above — an over-rewrite of bytes ada resolves to
// the variant — and it is what makes the `foreignObject` cell a special case
// rather than a rule. `<math><annotation-xml>` with no encoding is the control:
// that one really is foreign content, and hostshift is right there.
func TestR61TheOtherSixIntegrationPointsAreNotModelled(t *testing.T) {
	m := obfMatcher(t)
	for _, c := range []struct{ name, pre, post string }{
		{"svg > desc", `<svg><desc>`, `</desc></svg>`},
		{"math > mtext", `<math><mtext>`, `</mtext></math>`},
		{"math > mi", `<math><mi>`, `</mi></math>`},
		{"math > annotation-xml encoding=text/html",
			`<math><annotation-xml encoding="text/html">`, `</annotation-xml></math>`},
		{"math > annotation-xml encoding=application/xhtml+xml",
			`<math><annotation-xml encoding="application/xhtml+xml">`, `</annotation-xml></math>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := c.pre + `<script>fetch("` + r61Ref + `")</script>` + c.post
			if parserDecodesReferences(t, doc) {
				t.Fatalf("oracle: the parser *does* decode here, so this fixture no "+
					"longer tests the claim:\n  %s", doc)
			}
			out := rewriteHTML(t, m, doc, nil)
			if out != doc {
				t.Errorf("%s is an HTML integration point: the parser is back on HTML\n"+
					"  rules and a <script> there is script data, so ada resolves the raw\n"+
					"  bytes to the variant and nothing points at production.\n"+
					"  in:  %s\n  out: %s", c.name, doc, out)
			}
		})
	}
	// The control: no encoding, so it stays foreign content and the decode is right.
	doc := `<math><annotation-xml><script>fetch("` + r61Ref + `")</script></annotation-xml></math>`
	if !parserDecodesReferences(t, doc) {
		t.Fatal("oracle: annotation-xml with no encoding should still be foreign content")
	}
	if out := rewriteHTML(t, m, doc, nil); !strings.Contains(out, "wt-a--example.ddev.site") {
		t.Errorf("the control cell regressed: %s", out)
	}
}

// `<svg><title>` is an HTML integration point and RCDATA to the tokenizer at
// once, and the CSS view is the half that loses.
//
// The tokenizer classifies `<title>` by name and hands its whole content back as
// one raw-text token, so a `<style>` element inside `<svg><title>` never becomes
// SurfaceInlineStyle — and `cssEscapeLeak` is gated on exactly that name. The
// parser disagrees: `title` in the SVG namespace is an HTML integration point,
// so that `<style>` is a real HTML style element whose CSS tokenizer runs.
//
// ada, on what the CSS tokenizer hands the URL parser:
//
//	new URL("https://www.example.fi/x", "https://wt-a--example.ddev.site/").host
//	  === "www.example.fi"
//
// An `@import` is dereferenced, so this is test 28 rather than a byte-accuracy
// question — a production fetch from the developer's authenticated browser, with
// the census reporting a clean page. Round 60 taught this element to decode
// character references; the CSS axis of the same collision is still open.
func TestR61SVGTitleWithholdsTheCSSView(t *testing.T) {
	m := obfMatcher(t)
	const doc = `<svg><title><style>@import url(https\3a \2f \2f www.example.fi/x);</style></title></svg>`
	n, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	var isStyleElement func(*html.Node) bool
	isStyleElement = func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "style" {
			return true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if isStyleElement(c) {
				return true
			}
		}
		return false
	}
	if !isStyleElement(n) {
		t.Fatal("oracle: the parser did not build a <style> element here, so this " +
			"fixture no longer tests the claim")
	}
	if out := rewriteHTML(t, m, doc, nil); !strings.Contains(out, "wt-a--example.ddev.site") {
		t.Errorf("the parser builds a real <style> element inside <svg><title>, so\n"+
			"  its CSS tokenizer decodes \\3a and \\2f and ada resolves\n"+
			"  https://www.example.fi/x — this map's canonical, in an @import the\n"+
			"  browser fetches. hostshift saw one raw-text token named `title` and\n"+
			"  withheld the CSS view.\n  in:  %s\n  out: %s", doc, out)
	}
}

// The outermost integration point wins, and a nested one does not re-arm.
//
// The comment calls that deliberate — "a nested pair resolves to foreign, which
// over-decodes" — and nothing asserted it. Taking the innermost instead is not
// equivalent and errs toward the leak: in `<svg><desc><svg><mtext><script>` the
// inner `<mtext>` is an ordinary SVG element, so the parser is foreign there and
// decodes, and recording it as an integration point withholds the decode.
func TestR62TheOutermostIntegrationPointWins(t *testing.T) {
	m := r55Matcher(t)
	in := `<svg><desc><svg><mtext><script>fetch("https:&#47;&#47;` + r55Canonical +
		`/x")</script></mtext></svg></desc></svg>`
	if out := rewriteHTML(t, m, in, NewStats(false)); !strings.Contains(out, r55Variant) {
		t.Errorf("the inner <mtext> is an ordinary SVG element, so the parser "+
			"decodes there and this is %s — it was served live:\n  %s",
			r55Canonical, out)
	}
}

// The swallowed-<style> arm is for foreign content only.
//
// An ordinary `<title>` in `<head>` is RCDATA and no CSS tokenizer runs over it,
// so decoding escapes there rewrites bytes a reader sees in the tab — verbatim
// the over-rewrite round 60 fixed for text/plain, which is why the arm is gated
// on being inside <svg>/<math> at all. The gate had no test.
func TestR62TheSwallowedStyleArmIsForeignOnly(t *testing.T) {
	m := r55Matcher(t)
	in := `<title>Using &lt;style&gt; with https\3a \2f \2f ` + r55Canonical + `/x</title>`
	if out := rewriteHTML(t, m, in, NewStats(false)); out != in {
		t.Errorf("an ordinary <title> is not a stylesheet and nothing decodes a "+
			"CSS escape there, so these bytes reach the reader as written:\n"+
			"  in  %s\n  out %s", in, out)
	}
}

// A nested integration point does not narrow the outer one.
//
// `<svg><desc><svg><desc>` puts the parser back on HTML rules twice, and taking
// the *innermost* mark would be the more faithful reading — inside the inner
// `<desc>` the parser really is on HTML rules and decodes nothing. This model
// takes the outermost deliberately, so the inner subtree resolves to "foreign"
// and is over-decoded: every error in this model is pushed to the side that
// rewrites rather than the side that leaks, which is the trade §4.4 makes
// throughout and the reason an unbalanced `<svg>` is harmless.
//
// Asserted because it is a choice, not a consequence: taking the innermost is
// otherwise invisible to the suite, and it moves this model to the leaking side.
func TestR62ANestedIntegrationPointDoesNotNarrowTheOuterOne(t *testing.T) {
	m := r55Matcher(t)
	in := `<svg><desc><svg><desc><script>fetch("https:&#47;&#47;` + r55Canonical +
		`/x")</script></desc></svg></desc></svg>`
	if out := rewriteHTML(t, m, in, NewStats(false)); !strings.Contains(out, r55Variant) {
		t.Errorf("this model resolves a nested integration point to foreign and "+
			"over-decodes on purpose; taking the innermost mark instead withholds "+
			"the decode, which is the leaking direction:\n  %s", out)
	}
}
