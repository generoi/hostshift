package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
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

// Every content type the *response* side rewrites must be mapped back on the
// way in, or the two directions disagree about the same body.
//
// bodyKind listed the `text/` prefix and the JSON spellings, so text/xml was
// mapped back while application/xml, image/svg+xml, application/rss+xml,
// application/atom+xml and application/xhtml+xml were not — not even a plain
// variant origin. rewritableText rewrites the whole `+xml` family on the way
// out and rewritableHTML claims application/xhtml+xml, which is exactly the
// argument bodyKind's own comment makes for having added text/json.
//
// The enumeration is the thing being tested here. The previous round's finding
// was a body arm nobody had listed, and this test's predecessor asserted its
// property "on each of the four body arms" — an enumeration that was itself
// incomplete.
func TestEveryTypeTheResponseRewritesIsMappedBack(t *testing.T) {
	for _, ct := range []string{
		"text/plain", "text/xml", "text/html",
		"application/xml", "application/xhtml+xml", "image/svg+xml",
		"application/rss+xml", "application/atom+xml",
		"application/json", "application/x-www-form-urlencoded",
	} {
		t.Run(ct, func(t *testing.T) {
			h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			})
			body := `<a href="https://` + variantHost + `/x">t</a>`
			if strings.HasPrefix(ct, "application/json") {
				body = `{"u":"https://` + variantHost + `/x"}`
			}
			h.do(t, "POST", variantHost, "/wp-json/wp/v2/posts/1", ct, []byte(body))
			up, _ := io.ReadAll(h.seen.Body)
			if bytes.Contains(up, []byte(variantHost)) {
				t.Errorf("a variant hostname reached the upstream under %s, so it "+
					"would be written into the shared database:\n%s", ct, up)
			}
		})
	}
}

// Whatever the proxy rewrites, the census counts.
//
// spliceHostsIn discarded events, so every standalone entry point rewrote
// silently: the request line, the query, Referer/Origin, every request body,
// every response header, and every text/plain and XML response body. A sitemap
// with five CSS-escaped origins was rewritten with `--json` reporting none, and
// `--dry-run` — which §5.8 makes the tool you point at a canonical checkout to
// decide whether a site needs hostshift — answered "nothing to do" on the very
// shapes those views exist for. Third recurrence of the instrument-lies class,
// at the entry points the earlier fix did not enumerate.
//
// The obfuscated spellings are the point: the plain one was always counted,
// which is why nothing noticed.
func TestWhateverIsRewrittenIsCounted(t *testing.T) {
	for _, c := range []struct{ name, ctype, body string }{
		{"a css-escaped response body", "text/plain",
			`<loc>https\3a \2f \2f www.acmecorp.fi/x</loc>`},
		{"a percent-composed response body", "text/plain",
			`https%3A%5C%2F%5C%2Fwww.acmecorp.fi%2Fx`},
		{"an obfuscated separator in a response body", "text/plain",
			`https:\\www.acmecorp.fi/x`},
		{"a reference-encoded xml response body", "application/xml",
			`<loc>https:&#47;&#47;www.acmecorp.fi/x</loc>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", c.ctype)
				_, _ = w.Write([]byte(c.body))
			})
			_, got := h.get(t, variantHost, "/")
			if string(got) == c.body {
				t.Fatalf("nothing was rewritten, so this asserts nothing:\n%s", got)
			}
			if n := h.stats.Total(); n == 0 {
				t.Errorf("the body was rewritten and the census reports nothing:\n"+
					" in  %s\n out %s", c.body, got)
			}
		})
	}
}

// The same for the request direction, which is the side that writes to the
// shared database — so a silent rewrite there is a silent write.
func TestRequestSideRewritesAreCounted(t *testing.T) {
	for _, c := range []struct{ name, ctype, body string }{
		{"a css-escaped request body", "application/x-www-form-urlencoded",
			`content=https\3a \2f \2f ` + variantHost + `/a.png`},
		{"a reference-encoded request body", "text/plain",
			`https:&#47;&#47;` + variantHost + `/a.png`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			})
			h.do(t, "POST", variantHost, "/wp-json/wp/v2/posts/1", c.ctype, []byte(c.body))
			up, _ := io.ReadAll(h.seen.Body)
			if string(up) == c.body {
				t.Fatalf("nothing was rewritten, so this asserts nothing:\n%s", up)
			}
			if n := h.stats.Total(); n == 0 {
				t.Errorf("the request body was rewritten and the census reports "+
					"nothing:\n in  %s\n out %s", c.body, up)
			}
		})
	}
}

// A PHP-serialized request body is rewritten with its length prefixes repaired.
//
// `s:LEN:"DATA"` counts DATA in bytes, so a rewrite that changes a byte count
// leaves LEN stale and PHP refuses the whole structure — `false` where an
// options array should be, into a shared database, with no undo and no error.
//
// Skipping such a body was tried first and was worse. The response direction
// rewrites too, so the browser is served a blob whose prefix already counts the
// canonical host while the data holds the variant; passing the POST back
// untouched then wrote that stale prefix *and* the variant hostname upstream —
// both failures at once, where rewriting without repair at least round-tripped.
//
// Both spellings are here because a form percent-encodes: `options.php` sends
// `s%3A51%3A%22`, and the literal scanner the first attempt used never saw it.
// That is the settings flow the whole problem is about.
func TestSerializedRequestBodiesKeepTheirLengths(t *testing.T) {
	variantURL := "https://" + variantHost + "/wp-content/uploads"
	canonURL := "https://www.acmecorp.fi/wp-content/uploads"
	blob := func(u string) string {
		return `a:1:{s:3:"url";s:` + strconv.Itoa(len(u)) + `:"` + u + `";}`
	}

	for _, c := range []struct {
		name, ctype string
		body        string
		want        string
	}{
		{"a literal blob", "text/plain", blob(variantURL), blob(canonURL)},
		{"percent-encoded, the way a form sends it",
			"application/x-www-form-urlencoded",
			"option=" + url.QueryEscape(blob(variantURL)),
			"option=" + url.QueryEscape(blob(canonURL))},
		{"inside a JSON string value", "application/json",
			`{"b":` + jsonQuote(blob(variantURL)) + `}`,
			`{"b":` + jsonQuote(blob(canonURL)) + `}`},
		{"a multipart part", "multipart/form-data; boundary=BXX",
			"--BXX\r\nContent-Disposition: form-data; name=\"o\"\r\n\r\n" +
				blob(variantURL) + "\r\n--BXX--\r\n",
			"--BXX\r\nContent-Disposition: form-data; name=\"o\"\r\n\r\n" +
				blob(canonURL) + "\r\n--BXX--\r\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			})
			h.do(t, "POST", variantHost, "/wp-admin/options.php", c.ctype, []byte(c.body))
			up, _ := io.ReadAll(h.seen.Body)
			if string(up) != c.want {
				t.Errorf("\n got  %s\n want %s", up, up)
				t.Errorf("expected:\n%s", c.want)
			}
			assertLengthsAgree(t, string(up))
		})
	}
}

// assertLengthsAgree checks every `s:N:"…"` in s declares its real byte length,
// in either spelling. A stale prefix is what PHP refuses.
func assertLengthsAgree(t *testing.T, s string) {
	t.Helper()
	dec, err := url.QueryUnescape(s)
	if err != nil {
		dec = s
	}
	for _, hay := range []string{s, dec} {
		for i := 0; ; {
			j := strings.Index(hay[i:], `s:`)
			if j < 0 {
				break
			}
			j += i
			k := j + 2
			for k < len(hay) && hay[k] >= '0' && hay[k] <= '9' {
				k++
			}
			if k == j+2 || k+1 >= len(hay) || hay[k] != ':' || hay[k+1] != '"' {
				i = j + 2
				continue
			}
			n, _ := strconv.Atoi(hay[j+2 : k])
			start := k + 2
			if start+n >= len(hay) || hay[start+n] != '"' {
				t.Errorf("s:%d: does not end where it says it does, so PHP will "+
					"refuse this:\n%s", n, hay)
			}
			i = start + n
		}
	}
}

// The round trip a developer actually performs: the row is served through the
// proxy, edited in the browser, and posted back. It has to come home unchanged.
func TestASerializedRoundTripComesHomeUnchanged(t *testing.T) {
	row := `a:1:{s:3:"url";s:42:"https://www.acmecorp.fi/wp-content/uploads";}`

	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(row))
	})
	_, served := h.get(t, variantHost, "/wp-admin/options.php")
	if !bytes.Contains(served, []byte(variantHost)) {
		t.Fatalf("the response direction did not rewrite, so this asserts nothing:\n%s", served)
	}
	assertLengthsAgree(t, string(served))

	h.do(t, "POST", variantHost, "/wp-admin/options.php", "text/plain", served)
	up, _ := io.ReadAll(h.seen.Body)
	if string(up) != row {
		t.Errorf("the round trip did not come home:\n got  %s\n want %s", up, row)
	}
}

// Prose that merely looks serialized is not, and must still be rewritten.
func TestProseThatLooksSerializedIsStillRewritten(t *testing.T) {
	body := `The slide reads s:12:"hello world" — see https://` + variantHost + `/a`
	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	h.do(t, "POST", variantHost, "/x", "text/plain", []byte(body))
	up, _ := io.ReadAll(h.seen.Body)
	if bytes.Contains(up, []byte(variantHost)) {
		t.Errorf("a variant hostname reached the upstream because the body merely "+
			"looked serialized:\n%s", up)
	}
}

// jsonQuote is the minimal quoting these fixtures need.
func jsonQuote(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
}

// An identity map must not change a byte of a JSON body carrying a serialized
// value — test 24, on the one shape that defeats it.
//
// serializedJSONValue re-quoted unconditionally whenever a span was found.
// jsontext.AppendQuote does not escape `/` and wp_json_encode always does, so
// `\/\/` came back as `//`: six bytes changed under a map the proxy was asked
// to make a no-op, and enough to defeat the equality check that decides whether
// the upstream's ETag still describes the body.
func TestAnIdentityMapLeavesSerializedJSONAlone(t *testing.T) {
	ident, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://www.acmecorp.fi"),
		Variant:   origin.MustParse("https://www.acmecorp.fi"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"value":"a:1:{s:3:\"url\";s:22:\"https:\/\/www.acmecorp.fi\";}"}`,
		`{"v":"a:1:{s:1:\"u\";s:5:\"a\/b\/c\";}"}`,
	} {
		t.Run(body, func(t *testing.T) {
			h := newHarness(t, ident, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			})
			_, got := h.get(t, "www.acmecorp.fi", "/wp-json/x")
			if string(got) != body {
				t.Errorf("an identity map changed the bytes:\n in  %s\n out %s", body, got)
			}
		})
	}
}

// Test 24 across every arm, not just HTML.
//
// TestIdentityMapByteIdentity runs the corpus through the *HTML* filter only,
// so the JSON, text and request-body arms had no identity coverage at all —
// which is why two identity defects shipped: a length prefix re-emitted without
// its leading zero, and `\/\/` re-quoted as `//` on every JSON body carrying a
// serialized value. An identity map must be a no-op on every path the proxy
// has, in both directions.
func TestIdentityIsAByteNoOpOnEveryArm(t *testing.T) {
	ident, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://www.acmecorp.fi"),
		Variant:   origin.MustParse("https://www.acmecorp.fi"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string]string{
		"text/plain":                        `a:1:{s:3:"url";s:22:"https://www.acmecorp.fi";}`,
		"text/plain; leading zero":          `s:04:"abcd";`,
		"text/plain; lowercase percent":     `o=s%3a4%3a%22abcd%22%3b`,
		"application/json":                  `{"v":"a:1:{s:1:\"u\";s:22:\"https:\/\/www.acmecorp.fi\";}"}`,
		"application/json; escapes":         `{"u":"https:\/\/www.acmecorp.fi\/x"}`,
		"application/xml":                   `<urlset><loc>https://www.acmecorp.fi/a</loc></urlset>`,
		"text/html":                         `<a href="https://www.acmecorp.fi/a">t</a>`,
		"application/x-www-form-urlencoded": `u=https%3A%2F%2Fwww.acmecorp.fi%2Fa`,
	}
	for name, body := range bodies {
		ct := strings.SplitN(name, ";", 2)[0]
		t.Run(name+" response", func(t *testing.T) {
			h := newHarness(t, ident, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", ct)
				_, _ = w.Write([]byte(body))
			})
			_, got := h.get(t, "www.acmecorp.fi", "/x")
			if string(got) != body {
				t.Errorf("an identity map changed a %s response:\n in  %s\n out %s", ct, body, got)
			}
		})
		t.Run(name+" request", func(t *testing.T) {
			h := newHarness(t, ident, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			})
			h.do(t, "POST", "www.acmecorp.fi", "/x", ct, []byte(body))
			up, _ := io.ReadAll(h.seen.Body)
			if string(up) != body {
				t.Errorf("an identity map changed a %s request:\n in  %s\n out %s", ct, body, up)
			}
		})
	}
}
