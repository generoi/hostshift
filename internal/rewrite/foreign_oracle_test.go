package rewrite

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// A generated differential against the parser, over the whole foreign-content
// grammar rather than the shapes someone thought of.
//
// Rounds 60 through 64 each produced a test-28 leak in this model, and each was
// found by an auditor writing out a shape by hand — a counter that could not
// express re-entrancy, names matched without their vocabulary, a namespace read
// from the tag name, a `<style` prefix, a `<br/>` that missed a name test. Every
// one of them is a *cell in a grid*, and none of the hand-written tests covered
// the grid. This does.
//
// The property is one-sided on purpose. §4.4 chooses to over-decode when it must
// choose, so a document where hostshift rewrites something the browser would not
// resolve is not a failure here — it is the documented direction, and asserting
// the converse would encode as law the thing the model deliberately gets wrong.
// What is never allowed is the leak: an origin the parser resolves, left naming
// production.
//
// The oracle is `html.Parse`. "Resolvable" means the decoded canonical origin
// reaches a place the browser acts on: any text node, or any attribute value.
//
// A leak is the canonical *host* surviving in the output — not the decoded URL
// appearing in it. The output keeps whatever spelling the input used, so looking
// for the decoded form is a check that cannot fire, and the first version of
// this test did exactly that: it passed against five separately reverted fixes
// and reported 2310 documents of nothing.

// foreignPieces are the structural tokens the grammar walks. Each is one step a
// parser takes into, around, or out of foreign content.
var foreignPieces = []string{
	"<svg>", "<math>", "</svg>", "</math>",
	"<foreignObject>", "</foreignObject>",
	"<desc>", "<title>", "<mtext>", "<mi>",
	"<annotation-xml encoding=\"text/html\">",
	"<annotation-xml encoding=\"text/htmlish\">",
	"<p>", "<div>", "<br/>", "<img/>", "<font color=red>", "<font>",
	"<section>", "<g>", "<style-guide>",
	// 13.2.6's carve-out: inside a MathML text integration point these two are
	// processed by the foreign-content rules, so the parser is foreign again
	// below them. The grid missed round 65's leak until they were in it — an
	// alphabet that omits a rule cannot test it.
	"<mglyph>", "<malignmark>", "<span>",
}

// foreignPayloads place the origin somewhere the browser may or may not resolve
// it. The reference spelling is the one that separates the two: an HTML parser
// leaves it alone inside script and style, a foreign one decodes it.
var foreignPayloads = []string{
	`<script>a("` + oracleRef + `")</script>`,
	`<style>@import url("` + oracleRef + `")</style>`,
	`<img src="` + oracleRef + `">`,
	`<a href="` + oracleRef + `">t</a>`,
	oracleRef,
}

const (
	oracleRef   = `https:&#47;&#47;www.example.fi/x`
	oracleCanon = `https://www.example.fi/x`
	// The output keeps whatever spelling the input used, so a leak is the
	// canonical *host* surviving — not the decoded URL appearing. Looking for
	// the decoded form is a check that can never fire, and it is what the first
	// version of this test did: it passed against five reverted fixes.
	oracleHost = `www.example.fi`
)

// oracleResolves reports whether the parser puts the *decoded* canonical origin
// somewhere the browser acts on.
func oracleResolves(t *testing.T, doc string) bool {
	t.Helper()
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	live := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for _, a := range n.Attr {
			if strings.Contains(a.Val, oracleCanon) {
				live = true
			}
		}
		// Any text node, not only script and style. A decoded origin in a
		// `<script>` is one the page runs and in a `<style>` one it fetches; in
		// ordinary prose it is one a developer copy-pastes, which §4.4 opens with
		// and treats the same way. What separates the cells is whether the
		// parser decoded the reference at all.
		if n.Type == html.TextNode && strings.Contains(n.Data, oracleCanon) {
			live = true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return live
}

func TestForeignContentNeverLeaksAgainstTheParser(t *testing.T) {
	m := obfMatcher(t)
	var docs []string
	// Depth 3, because depth 2 is not enough and I assumed otherwise once: the
	// round-62 leak is `<svg><math><mi><script>`, and it takes all three steps —
	// one to enter a vocabulary, one to be read in the wrong one, one to be
	// mistaken for an integration point.
	//
	// Measured, by reverting each fix in a copy of the tree and running this:
	// the vocabulary read from the tag name (11 documents), a mismatched end tag
	// popping the innermost element (20), integration points matched by name
	// alone (20), no breakout list (5), a nested `<svg>` in a `<title>` not
	// treated as foreign (2), `<style` matched as a prefix (3), and both of the
	// fixes this test itself found (2 and 1). At depth 2 every one of those
	// passed.
	for _, a := range foreignPieces {
		for _, b := range foreignPieces {
			for _, c := range foreignPieces {
				for _, p := range foreignPayloads {
					docs = append(docs, a+b+c+p)
				}
			}
			for _, p := range foreignPayloads {
				docs = append(docs, a+b+p)
			}
		}
		for _, p := range foreignPayloads {
			docs = append(docs, a+p)
		}
	}

	var leaks []string
	live, over := 0, 0
	for _, doc := range docs {
		if !oracleResolves(t, doc) {
			// The browser does not resolve it here. Rewriting anyway is the
			// over-decode §4.4 accepts; count it, do not fail on it.
			if !strings.Contains(rewriteHTML(t, m, doc, nil), oracleHost) {
				over++
			}
			continue
		}
		live++
		if out := rewriteHTML(t, m, doc, nil); strings.Contains(out, oracleHost) {
			if len(leaks) < 20 {
				leaks = append(leaks, "  in:  "+doc+"\n  out: "+out)
			}
		}
	}
	if live == 0 {
		t.Fatal("no document in the grid resolves the canonical origin, so this " +
			"asserts nothing — the fixtures or the oracle have drifted")
	}
	if len(leaks) > 0 {
		t.Errorf("%d of %d documents the parser resolves kept the canonical origin.\n"+
			"Each is a dereferenceable production origin reaching the browser — "+
			"test 28.\n%s", len(leaks), live, strings.Join(leaks, "\n"))
	}
	t.Logf("%d documents, %d the parser resolves, %d over-decodes (accepted, §4.4)",
		len(docs), live, over)
}
