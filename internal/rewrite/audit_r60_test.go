package rewrite

import (
	"strings"
	"testing"
)

// Round 60, on b9b5c0b, auditing the HTML tokenizer's surface dispatch: the
// 26 x 5 grid of (container, corpus encoding) that decides which element,
// attribute and token kind reaches which decoder.
//
// Most of the grid agrees with the parser. This is the row that does not.

// A raw-text element is not one thing, and the model says it is.
//
// `rawTextNames` is one list — script, style, textarea, title, iframe, noembed,
// noframes, noscript, plaintext, xmp — and everything on it that is not script
// or style becomes `SurfaceRawText`, whose doc comment reads "markup, which no
// string decoder touches" and which therefore gets no reference view unless the
// document is XHTML or the element is inside <svg>/<math>.
//
// The HTML parser does not have one state for that list. It has three:
//
//   - RAWTEXT — style, xmp, iframe, noembed, noframes, and noscript when
//     scripting is enabled. Character references are *not* decoded. The model
//     is right about these.
//   - RCDATA — title and textarea. Character references *are* decoded
//     (WHATWG HTML 13.2.5.2, "RCDATA state", which enters the character
//     reference state on `&`).
//   - Markup — noscript when scripting is disabled, which the same standard
//     makes an ordinary parsed subtree.
//
// So `<title>` and `<textarea>` are given the surface whose whole justification
// is that nothing decodes in them, and the one thing that does decode in them
// is the one view that is withheld.
//
// ada, with the variant origin as base, on the decoded form the parser hands on:
//
//	new URL("https://www.example.fi/x", base).host === "www.example.fi"
//
// That is this map's canonical. The raw form is byte-identical out: the byte
// matcher's three encodings do not model a reference, `normaliseURLLeak` does
// not decode one, and §4.4's straggler sweep is the same byte matcher, so it
// cannot see it either — the census reports a clean page.
//
// What it costs, stated at its ceiling rather than above it: a `<title>` or a
// `<textarea>` value is not dereferenced, so this is not test 28 on its own. It
// is test 28 for `<noscript>`, whose content a scripting-disabled browser parses
// as markup and whose classic payload is exactly an analytics `<img>` — a
// production fetch from the developer's authenticated browser. And on all three
// it is a census that says zero about a surface where the browser and the engine
// read different bytes, which is the state §5.2 puts the every-attribute scan
// there to prevent.
func TestR60RCDATAElementsDecodeReferences(t *testing.T) {
	m := obfMatcher(t)
	const ref = `https:&#47;&#47;www.example.fi/x`
	for _, c := range []struct{ name, in, why string }{
		{
			"title",
			`<title>` + ref + `</title>`,
			"RCDATA: the parser decodes the reference into document.title",
		},
		{
			"textarea",
			`<textarea name="c">` + ref + `</textarea>`,
			"RCDATA: the parser decodes the reference into the field's value, " +
				"which is what the editor shows and what the form posts back",
		},
		{
			"noscript",
			`<noscript><img src="` + ref + `"></noscript>`,
			"with scripting disabled the parser reads this subtree as markup, " +
				"and the browser fetches the decoded src — test 28",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := rewriteHTML(t, m, c.in, NewStats(false))
			if !strings.Contains(out, "wt-a--example.ddev.site") {
				t.Errorf("%s\nada resolves the decoded form to www.example.fi, this map's\n"+
					"canonical, and the value went out byte-identical:\n in  %q\n out %q",
					c.why, c.in, out)
			}
		})
	}

	// The control: the elements the model *is* right about. RAWTEXT decodes
	// nothing, so a reference there is two dozen literal characters and
	// rewriting it would be the mirror error — a value nothing resolves,
	// changed on a page that was already correct.
	for _, name := range []string{"iframe", "xmp", "noembed", "noframes"} {
		t.Run("raw text "+name+" is left alone", func(t *testing.T) {
			in := "<" + name + ">" + ref + "</" + name + ">"
			if out := rewriteHTML(t, m, in, NewStats(false)); out != in {
				t.Errorf("<%s> is RAWTEXT: the parser decodes no reference in it, so a\n"+
					"browser resolves nothing here and nothing may change:\n in  %q\n out %q",
					name, in, out)
			}
		})
	}
}
