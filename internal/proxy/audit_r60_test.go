package proxy

import (
	"net/http"
	"strings"
	"testing"
)

// Round 60, on b9b5c0b, auditing the HTML tokenizer's surface dispatch — which
// element, attribute and token kind reaches which decoder — and the same
// question asked of the standalone body arms, where a *content type* rather
// than an element picks the decoders.
//
// The grid is 26 containers x 5 corpus encodings; these are the cells where
// hostshift and the browser disagree.

// The text/plain response arm decodes CSS escapes, and nothing decodes them
// there.
//
// Round 59 split the header surface on exactly this question and wrote the rule
// down: "A browser following a Location runs the URL parser and nothing else."
// The same sentence is true of a text/plain body — a browser renders it as
// characters, resolves nothing in it, and never runs a CSS tokenizer over it —
// but `surfaceDecodesCSS` classifies only `SurfaceResponseHeader`, and every
// other name falls through its default to true. proxy.go's text arm passes
// `SurfaceText`, so `stripForCSS` runs.
//
// Three lines above it the same call already splits by content type for the
// *reference* view, and says why:
//
//	// text/plain keeps HostLeaks: nothing parses references in plain text, so
//	// leaving them is correct there.
//
// The CSS view needs that split for the same reason and does not get it, so one
// surface name gives one answer to two content types.
//
// ada, with the variant origin as base, on the bytes as served — no CSS
// tokenizer, because there is none on this arm:
//
//	new URL("https\\3a \\2f \\2f www.acmecorp.fi\\2f x", base).host ===
//	  "wt-a--acmecorp.ddev.site"
//
// The host is the variant. Nothing in that string points at production, so
// test 28 asks for nothing here and the oracle's second half — "resolves
// anywhere else, so it must not change" — forbids touching it. hostshift
// rewrites it anyway, splicing the variant into an escape spelling the reader
// then sees literally, and it does that to 3,876 of the corpus's 6,300
// CSS-encoded shapes on this arm: the same 3,876 round 59 counted for the
// Location twin of this defect.
func TestR60TextPlainDoesNotDecodeCSSEscapes(t *testing.T) {
	// The CSS spelling of https://www.acmecorp.fi/x. In a stylesheet the
	// tokenizer decodes it; in a text/plain body nothing does.
	const css = `https\3a \2f \2f www.acmecorp.fi\2f x`

	t.Run("text/plain", func(t *testing.T) {
		body := "See " + css + " for details.\n"
		h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		})
		_, got := h.get(t, variantHost, "/readme.txt")
		if string(got) != body {
			t.Errorf("a text/plain body is not CSS: a browser resolves\n"+
				"  %q\nto host wt-a--acmecorp.ddev.site (ada), so nothing here points at\n"+
				"production and nothing may change. The CSS view ran anyway:\n"+
				" in  %q\n out %q", css, body, string(got))
		}
	})

	// The control, and the reason the view is armed at all: an SVG's <style> is
	// read by a CSS tokenizer, so the same bytes there *must* be rewritten.
	// Whatever splits text/plain off has to leave this one alone.
	t.Run("image/svg+xml", func(t *testing.T) {
		body := `<svg xmlns="http://www.w3.org/2000/svg"><style>` +
			`a{background:url(` + css + `)}</style></svg>`
		h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/svg+xml")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		})
		_, got := h.get(t, variantHost, "/icon.svg")
		if !strings.Contains(string(got), variantHost) {
			t.Errorf("an SVG <style> is CSS and a browser fetches the decoded URL, so\n"+
				"this one must still be rewritten:\n in  %q\n out %q", body, string(got))
		}
	})
}
