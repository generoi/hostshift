package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Round 56, the same grid one layer up: the arms the proxy really runs.
//
// TestEveryRequestBodyArmReadsEverySpelling crosses six hand-written spellings
// with four arms, and its one percent row is `https%3A%5C%2F%5C%2F…` — percent
// over a *JSON* escape. The spelling it does not name is percent over a *CSS*
// escape, and that is the one the engine cannot read: rewriteAll composes the
// percent view with the JSON view (gated on the literal `%5Cu`) and with
// nothing else, so a `%5C3a` goes upstream whole.
//
// The value is a variant origin. A CSS tokenizer decodes `\3a ` to `:` and
// `\2f ` to `/` (css-syntax-3 §4.3.7, one trailing whitespace consumed), and
// ada then reads the host:
//
//	new URL("https://wt-a--acmecorp.ddev.site/a.png").host === "wt-a--acmecorp.ddev.site"
//
// The percent layer is not an extra hop of obfuscation, it is what PHP undoes
// before it stores the field:
//
//	decodeURIComponent("https%5C3a+%5C2f+…".replace(/\+/g," "))
//	  === "https\3a \2f \2f wt-a--acmecorp.ddev.site\2f a.png"
//
// so what `wp_options` holds is the variant hostname, and production then
// serves a `.ddev.site` URL to real visitors. §4.3, with no undo.
//
// The producer is the one rewriteAll's own comment names: cssEscapeLeak splices
// the variant into a style attribute that already spells its URL in CSS
// escapes, and `post.php` posts that field back percent-encoded.
func TestR56EveryRequestArmReadsAPercentEncodedCSSEscape(t *testing.T) {
	css := `https\3a \2f \2f ` + variantHost + `\2f a.png`
	css6 := `https\00003a\00002f\00002f` + variantHost + `\00002fa.png`
	for _, sp := range []struct{ name, url string }{
		{"css-escaped", css},
		{"css-escaped, six hex digits", css6},
		{"percent over css-escaped",
			strings.NewReplacer(`\`, "%5C", " ", "%20").Replace(css)},
		{"percent over css-escaped, a form's plus for the space",
			strings.NewReplacer(`\`, "%5C", " ", "+").Replace(css)},
		{"percent over css-escaped, six hex digits",
			strings.NewReplacer(`\`, "%5C").Replace(css6)},
	} {
		for _, arm := range []struct {
			name, ctype string
			wrap        func(string) string
		}{
			{"text-plain", "text/plain", func(s string) string { return s }},
			{"urlencoded", "application/x-www-form-urlencoded",
				func(s string) string { return "content=" + s }},
			{"json", "application/json",
				func(s string) string { return `{"content":"` + strings.ReplaceAll(s, `\`, `\\`) + `"}` }},
			{"multipart", "multipart/form-data; boundary=BXX", func(s string) string {
				return "--BXX\r\nContent-Disposition: form-data; name=\"content\"\r\n\r\n" +
					`<div style="background:url(` + s + `)"></div>` + "\r\n--BXX--\r\n"
			}},
		} {
			t.Run(sp.name+"/"+arm.name, func(t *testing.T) {
				h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(200)
				})
				res, rb := h.do(t, "POST", variantHost, "/wp-json/wp/v2/posts/1", arm.ctype,
					[]byte(arm.wrap(sp.url)))
				if h.seen == nil {
					t.Fatalf("the upstream was never reached: status %d, body %q", res.StatusCode, rb)
				}
				up, _ := io.ReadAll(h.seen.Body)
				if bytes.Contains(up, []byte(variantHost)) {
					t.Errorf("a variant hostname reached the upstream, so it would be "+
						"written into the shared database:\n%s", up)
				}
			})
		}
	}
}
