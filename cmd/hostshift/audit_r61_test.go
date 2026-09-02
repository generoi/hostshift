package main

import (
	"strings"
	"testing"
)

// Round 61, on 33cb2a6: `rewrite` is the third engine, and round 60 changed two.
//
// 33cb2a6 made `surfaceDecodesCSS(SurfaceText)` false — correctly, because
// nothing decodes a CSS escape in text/plain — and then split the text arm by
// media type so the XML family keeps the CSS view: "SurfaceXMLText for the XML
// family, SurfaceText for the rest, and the scorer makes the same choice by the
// same question."
//
// It names two call sites. There are three. `cmdRewrite`'s `rewritableText` arm
// is a line-for-line copy of the proxy's, down to the comment, and it still
// passes `rewrite.SurfaceText` on both branches — so the change that switched
// the CSS view off by name switched it off for every feed, sitemap and SVG this
// command is given, in the one direction round 60 went out of its way to keep.
//
// Measured against b9b5c0b: this same body rewrote under all three media types
// before the commit and rewrites under none of them after it. The command
// documents itself as "the same engine" and PLAN §7 leans on that; here it
// disagrees with the proxy about the identical bytes, and answers
// `"rewrites": {}` — the JSON `check`'s leak scan reads as a count of zero.
//
// ada, with the variant origin as base, on what a CSS tokenizer hands the URL
// parser after it unescapes `\3a` and `\2f`:
//
//	new URL("https://www.acme.fi/a.png", "https://wt-a--acme.ddev.site/").host
//	  === "www.acme.fi"
//
// That is this map's canonical, in a `background: url(…)` the browser fetches —
// test 28, with the tool a developer reaches for to diagnose it reporting a
// clean body.
func TestR61RewriteXMLArmKeepsTheCSSView(t *testing.T) {
	mapFlag := "https://www.acme.fi=https://wt-a--acme.ddev.site"
	const svg = `<svg xmlns="http://www.w3.org/2000/svg"><style>` +
		`body{background:url(https\3a \2f \2f www.acme.fi/a.png)}</style></svg>`

	for _, mt := range []string{"image/svg+xml", "application/rss+xml", "text/xml"} {
		t.Run(mt, func(t *testing.T) {
			code, out, errOut := run(t, svg, cmdRewrite,
				"--map", mapFlag, "--type", mt, "--json")
			if code != exitOK {
				t.Fatalf("exit %d\n%s", code, errOut)
			}
			if !strings.Contains(out, "wt-a--acme.ddev.site") {
				t.Errorf("an SVG <style> is CSS — the split 33cb2a6 made by media type\n"+
					"  in the proxy and in the scorer says so — and its CSS tokenizer\n"+
					"  resolves this to https://www.acme.fi/a.png, this map's canonical.\n"+
					"  `rewrite` is the third copy of that arm and still names the\n"+
					"  surface `text`, which the same commit made CSS-free.\n"+
					"  out:  %s\n  json: %s", strings.TrimSpace(out), strings.TrimSpace(errOut))
			}
			if strings.Contains(errOut, `"rewrites": {}`) {
				t.Errorf("--json reported no rewrites at all over a body carrying a\n" +
					"  dereferenceable production origin; that empty object is what\n" +
					"  `ddev hostshift check`'s scan reads as a leak count of zero.")
			}
		})
	}
}

// And the census name: the same body under the same media type must be reported
// on the same surface by all three engines, or `--explain` and the scorer's
// per-surface counters describe different bodies.
func TestR61RewriteNamesTheXMLSurfaceTheProxyNames(t *testing.T) {
	mapFlag := "https://www.acme.fi=https://wt-a--acme.ddev.site"
	const feed = `<rss><channel><link>https://www.acme.fi/x</link></channel></rss>`
	_, _, errOut := run(t, feed, cmdRewrite,
		"--map", mapFlag, "--type", "application/rss+xml", "--json")
	if !strings.Contains(errOut, "xml-text") {
		t.Errorf("the proxy and internal/corpus both record an XML-family body on\n"+
			"  rewrite.SurfaceXMLText (\"xml-text\"); `rewrite` still records it as\n"+
			"  \"text\", so the census a developer reads here names a different\n"+
			"  surface from the one the proxy logs for the identical bytes.\n"+
			"  json: %s", strings.TrimSpace(errOut))
	}
}
