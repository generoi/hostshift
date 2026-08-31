package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Every spelling the response direction can emit, the request direction must be
// able to read — asserted *through the proxy*, on each of the four body arms.
//
// rewrite's own TestForwardEmissionsAreReadableInReverse calls HostLeaksBack
// directly and its comment says "which is what the proxy uses on a request
// body". That premise was false at one of the four arms: multipart still called
// HostLeaks, which has no reference view and no composed refs→CSS view. Because
// that test never goes through the proxy it could not notice — the fourth time
// in this project a test has been blind along its own dimension.
//
// A multipart POST is what any form with a file field sends: the media library,
// an editor with an attachment, Gravity Forms. The part that leaked carries a
// style attribute, which is where a page builder's background images live.
func TestEveryRequestBodyArmReadsEverySpelling(t *testing.T) {
	spellings := []struct{ name, url string }{
		{"plain", "https://" + variantHost + "/a.png"},
		{"json-escaped", `https:\/\/` + variantHost + `/a.png`},
		{"css-escaped", `https\3a \2f \2f ` + variantHost + `/a.png`},
		{"reference-encoded", `https:&#47;&#47;` + variantHost + `/a.png`},
		{"references spelling css", `https&#92;3a &#92;2f &#92;2f ` + variantHost + `/a.png`},
		{"percent-encoded", `https%3A%5C%2F%5C%2F` + variantHost + `%2Fa.png`},
	}

	for _, sp := range spellings {
		for _, arm := range []struct{ name, ctype string; wrap func(string) string }{
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

// A malformed JSON request body has to fall back to the sweep, the way a
// malformed *response* body always has.
//
// RewriteJSON returns the document untouched whenever jsontext rejects it and
// logs a WARN. On the response side SweepBytes catches what the structured pass
// could not reach; on the request side the WARN was all that happened, so the
// variant hostname went upstream. The asymmetry was backwards: a leak into a
// response is one page view, a leak into a request is written down.
//
// None of these shapes is exotic. Invalid UTF-8 is what a Windows-1252 paste
// produces, and a truncated body is what a dropped connection produces.
func TestAMalformedJSONRequestBodyIsStillSwept(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"invalid utf-8", `{"u":"https://` + variant + `/x","t":"caf` + "\xe9" + `"}`},
		{"a duplicate member", `{"a":1,"a":2,"u":"https://` + variantHost + `/x"}`},
		{"a trailing comma", `{"u":"https://` + variant + `/x",}`},
		{"a truncated document", `{"u":"https://` + variant + `/x"`},
		{"a raw control character", "{\"u\":\"https://" + variantHost + "/x\",\"t\":\"a\tb\"}"},
		{"a bare NaN", `{"u":"https://` + variant + `/x","n":NaN}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			})
			h.do(t, "POST", variantHost, "/wp-json/wp/v2/posts/1", "application/json",
				[]byte(c.body))
			up, _ := io.ReadAll(h.seen.Body)
			if bytes.Contains(up, []byte(variantHost)) {
				t.Errorf("a variant hostname reached the upstream through a body the "+
					"structured pass declined:\n%s", up)
			}
		})
	}
}
