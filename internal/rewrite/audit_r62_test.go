package rewrite

import (
	"strings"
	"testing"
)

// Round 62, on ea9e212, auditing the axis round 61 drew and then dropped: the
// *namespace* of a foreign-content integration point.
//
// Round 61's finding was that `foreignObject` is one of seven, and its fix
// widened one name to nine. Its own commit message states the rule correctly —
// "SVG has three (`foreignObject`, `desc`, `title`) and MathML has five text
// integration points (`mi`, `mo`, `mn`, `ms`, `mtext`) plus `annotation-xml`" —
// and the code that implements it compares the name alone, in one flat list,
// with no reference to which of `<svg>` or `<math>` is open.
//
// HTML 13.2.6 scopes both halves by namespace. An HTML integration point is an
// **SVG element** named foreignObject, desc or title; a MathML text integration
// point is a **MathML element** named mi, mo, mn, ms or mtext. A name from the
// other list is an ordinary unknown element in the namespace it appears in, and
// the parser stays in foreign content there — where it decodes character
// references in `<script>` and `<style>`, which is the whole reason this model
// exists.
//
// The direction of the error is the leaking one, and it is the same leak round
// 61 fixed:
//
//	<svg><mi><script>fetch("https:&#47;&#47;www.example.fi/x")</script>
//
// `mi` inside `<svg>` is not an integration point, so the `<script>` is an SVG
// script element — which runs — and the parser decodes `&#47;&#47;` to `//`
// before it runs. hostshift records `mi` as an integration point, concludes
// "HTML rules", withholds the reference view, and serves the canonical origin
// through untouched. That is test 28: a dereferenceable production origin in the
// browser, from a script the page executes, with the developer's session on it.
//
// The oracle for "which rules is the parser under, and in which namespace does
// the element land" is golang.org/x/net/html's *parser* — the oracle round 61
// used — which implements 13.2.6 including the integration-point rules:
//
//	html.Parse(`<svg><mi><script>a&#47;&#47;b</script>`)
//	  -> script node, Namespace == "svg", text == "a//b"        // decoded
//	html.Parse(`<math><mi><script>a&#47;&#47;b</script>`)
//	  -> script node, Namespace == ""   , text == "a&#47;&#47;b" // verbatim
//
// and ada, through Node's URL, says what the decoded bytes then mean: with the
// variant origin as base, `https://www.example.fi/x` resolves to host
// `www.example.fi` and the reference-spelled form resolves to the variant, which
// is test 28 in both directions at once.

const r62Ref = `https:&#47;&#47;www.example.fi/x`

// The grid: two foreign roots by nine integration-point names by two consumers.
//
// Forty cells, and it is the cross product rather than a list of instances
// because that is what tells a name from a rule. Twenty-three of them were wrong
// at ea9e212 and every one was wrong in the leaking direction, because dropping
// the namespace can only ever *add* integration points — and an integration
// point is the state in which hostshift declines to decode.
//
// Only the leaking half is asserted. Where the parser does not decode and
// hostshift rewrites anyway, the result is an over-rewrite, which §4.4 chooses
// on purpose and which this file must not freeze into a contract.
func TestR62IntegrationPointsAreNamespaceScoped(t *testing.T) {
	m := obfMatcher(t)
	names := []string{
		"foreignObject", "desc", "title", "mi", "mo", "mn", "ms", "mtext",
		`annotation-xml encoding="text/html"`,
	}
	consumers := map[string]string{
		"script": `<script>fetch("` + r62Ref + `")</script>`,
		"style":  `<style>@import url("` + r62Ref + `");</style>`,
	}
	cells := 0
	for _, outer := range []string{"svg", "math"} {
		for _, n := range names {
			short := strings.Fields(n)[0]
			for _, ck := range []string{"script", "style"} {
				cells++
				doc := "<" + outer + "><" + n + ">" + consumers[ck] +
					"</" + short + "></" + outer + ">"
				t.Run(outer+"/"+short+"/"+ck, func(t *testing.T) {
					if !parserDecodesReferences(t, doc) {
						// The parser is under HTML rules here, so declining to
						// decode is correct and there is nothing to assert.
						t.Skipf("oracle: the parser does not decode here: %s", doc)
					}
					out := rewriteHTML(t, m, doc, nil)
					if !strings.Contains(out, "wt-a--example.ddev.site") {
						t.Errorf("`%s` is not an integration point inside <%s> — it is an\n"+
							"  ordinary element in that namespace, so the parser is still in\n"+
							"  foreign content and decodes the references to\n"+
							"  https://www.example.fi/x, this map's canonical. hostshift read\n"+
							"  the name without the namespace, called it an integration point,\n"+
							"  and withheld the reference view: the canonical origin went to\n"+
							"  the browser untouched, in an element it dereferences.\n"+
							"  in:  %s\n  out: %s", short, outer, doc, out)
					}
				})
			}
		}
	}
	if cells != 36 {
		t.Fatalf("the grid is meant to be 2 roots x 9 names x 2 consumers, got %d", cells)
	}
}

// A MathML name inside `<svg>` disarms the rest of that subtree.
//
// The mark is cleared by the matching end tag or by `</svg>` dropping below the
// depth that set it, so an unbalanced `<svg><mi>` — which the parser treats as
// an ordinary unknown SVG element and never leaves — keeps every later
// `<script>` and `<style>` in that subtree unrewritten. This is the blast-radius
// shape round 61's own test names, arriving through the new name list rather
// than through the old counter.
func TestR62AMathMLNameInSVGDisarmsTheSubtree(t *testing.T) {
	m := obfMatcher(t)
	const doc = `<svg><mi>x<script>fetch("` + r62Ref + `")</script></svg>`
	if !parserDecodesReferences(t, doc) {
		t.Fatalf("oracle: the parser does not decode here: %s", doc)
	}
	out := rewriteHTML(t, m, doc, nil)
	if !strings.Contains(out, "wt-a--example.ddev.site") {
		t.Errorf("an unbalanced <mi> inside <svg> — not an integration point in\n"+
			"  that namespace — left the mark set, so a later <script> in the same\n"+
			"  <svg> whose references the parser decodes to\n"+
			"  https://www.example.fi/x went out untouched.\n  in:  %s\n  out: %s", doc, out)
	}
}

// `encoding` is an attribute, and a substring search is not an attribute.
//
// htmlEncoding searches the whole start tag for the bytes "encoding", so
// `data-encoding`, `xencoding` and an unrelated attribute whose *value* contains
// `encoding=text/html` all satisfy it. Each false positive records an
// integration point where the parser has none, which withholds the reference
// view — the leaking direction again.
//
// It also compares by prefix where the parser compares for equality
// (`text/htmlx` is not `text/html`), and its TrimLeft crosses the opening quote
// and the spaces in one step, so a quoted `" text/html"` matches where the
// parser sees a value that begins with a space and is not one of the two.
//
// The same commit anchored `hs_db_command`'s dotenv scan to an assignment for
// precisely this reason, and wrote this one as a bare `bytes.Index`.
func TestR62AnnotationXMLEncodingIsAnAttribute(t *testing.T) {
	m := obfMatcher(t)
	for _, tag := range []string{
		`<annotation-xml data-encoding="text/html">`,
		`<annotation-xml xencoding="text/html">`,
		`<annotation-xml alt="encoding=text/html">`,
		`<annotation-xml encoding="text/htmlx">`,
		`<annotation-xml encoding="text/html-ish">`,
		`<annotation-xml encoding=" text/html">`,
	} {
		t.Run(tag, func(t *testing.T) {
			doc := `<math>` + tag + `<script>fetch("` + r62Ref +
				`")</script></annotation-xml></math>`
			if !parserDecodesReferences(t, doc) {
				t.Fatalf("oracle: the parser does not decode here, so this fixture "+
					"no longer tests the claim:\n  %s", doc)
			}
			out := rewriteHTML(t, m, doc, nil)
			if !strings.Contains(out, "wt-a--example.ddev.site") {
				t.Errorf("this <annotation-xml> carries no encoding the parser accepts,\n"+
					"  so it is ordinary MathML and the parser stays in foreign content\n"+
					"  and decodes the references to https://www.example.fi/x. hostshift\n"+
					"  matched the bytes \"encoding\" somewhere in the tag, called it an\n"+
					"  HTML integration point, and served the canonical origin through.\n"+
					"  in:  %s\n  out: %s", doc, out)
			}
		})
	}
}

// The control: where the namespace *does* make it an integration point, the
// reference view stays withheld.
//
// Without this the fix above could be "never treat anything as an integration
// point", which is round 60's pre-state and which over-rewrites a JS string
// literal the browser reads verbatim — the error this file exists to prevent, in
// the other direction.
func TestR62IntegrationPointsInTheirOwnNamespaceStillWithhold(t *testing.T) {
	m := obfMatcher(t)
	for _, doc := range []string{
		`<svg><foreignObject><script>fetch("` + r62Ref + `")</script></foreignObject></svg>`,
		`<svg><desc><script>fetch("` + r62Ref + `")</script></desc></svg>`,
		`<math><mtext><script>fetch("` + r62Ref + `")</script></mtext></math>`,
		`<math><mi><script>fetch("` + r62Ref + `")</script></mi></math>`,
		`<math><annotation-xml encoding="text/html"><script>fetch("` + r62Ref +
			`")</script></annotation-xml></math>`,
	} {
		t.Run(doc, func(t *testing.T) {
			if parserDecodesReferences(t, doc) {
				t.Fatalf("oracle: the parser decodes here, so this is not a control: %s", doc)
			}
			out := rewriteHTML(t, m, doc, nil)
			if strings.Contains(out, "wt-a--example.ddev.site") {
				t.Errorf("the parser is under HTML rules here and reads these bytes\n"+
					"  verbatim — nothing in this script points at production — and\n"+
					"  hostshift rewrote them anyway, corrupting a JS string literal.\n"+
					"  in:  %s\n  out: %s", doc, out)
			}
		})
	}
}
