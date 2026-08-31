package corpus

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/proxy"
	"github.com/generoi/hostshift/internal/rewrite"
)

// The scorer must produce what the proxy produces, for every content type and
// body shape — asserted against the real proxy rather than against a belief
// about it.
//
// applyLikeTheProxy was written to end a class of bug where the scorer ran a
// different pipeline than the proxy, and it reintroduced the same class in a
// new place: it ran one of the proxy's three text passes, so a plain
// unencoded origin sitting after a newline scored clean. stripForURL *deletes*
// tab, LF and CR — correct for a single URL value, wrong for a whole document,
// where those bytes are separators. Its arm boundaries had drifted too, so a
// text/markdown body the proxy passes through was rewritten by the scorer and
// turned a healthy run RED.
//
// Comparing prose about the dispatch cannot catch that. Running both can.
//
// The remaining honest limit: dropping SweepBytes alone is still invisible,
// because Rewrite is a superset of RewriteText on this arm and nothing here
// needs the sweep that the earlier pass does not already catch. Dropping
// RewriteText alone is caught, by the trailing-root-label body below. The arm
// *boundaries* are pinned tightest — dropping isTextArm for the looser
// prefix/suffix test it replaced fails 37 of these combinations.
func TestTheScorerMatchesTheProxy(t *testing.T) {
	m, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://www.canon.test"),
		Variant:   origin.MustParse("https://v.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	bodies := []struct{ name, body string }{
		{"plain, inline", `see https://www.canon.test/a here`},
		// The shape that scored clean: a newline immediately before the origin.
		{"plain, after a newline", "Katso lisaa\nhttps://www.canon.test/a"},
		{"plain, after a tab", "url\thttps://www.canon.test/a"},
		{"plain, after a CR", "url\rhttps://www.canon.test/a"},
		{"reference-encoded", `<loc>https:&#47;&#47;www.canon.test/a</loc>`},
		{"css-escaped", `a{background:url(https\3a \2f \2f www.canon.test/a)}`},
		{"percent-composed", `https%3A%5C%2F%5C%2Fwww.canon.test%2Fa`},
		{"inside CDATA after a newline", "<d><![CDATA[x\nhttps://www.canon.test/a]]></d>"},
		{"an rss link", `<rss><channel><item><link>https://www.canon.test/a</link></item></channel></rss>`},
		{"a duplicate JSON member", `{"a":"https://www.canon.test/a","a":"b"}`},
		// The async-upload shape: JSON served as text/plain, with the origin
		// written in \uXXXX escapes that only RewriteJSON decodes.
		{"json escapes under a text label",
			`{"success":true,"data":{"url":"http://\u0077ww.canon.test/x.png"}}`},
		// Serialized payloads, in both spellings and nested. The scorer and the
		// proxy were wrapped in RepairSerialized by different commits, and none
		// of the bodies above contains an `s:N:"…"` so nothing noticed.
		//
		// The lengths are computed, not written by hand. My first attempt hard-
		// coded them, got one wrong by a single byte, and the span therefore
		// never parsed — so the fixtures exercised nothing and the mutation that
		// should have failed passed. A fixture that silently does not reach the
		// code it names is the failure mode this whole loop keeps finding.
		{"a serialized blob", ser("u", canonA)},
		{"a serialized blob, percent-encoded", "o=" + serPct("u", canonA)},
		{"a nested serialized blob", serRaw("o", ser("u", canonA))},
		{"a serialized blob beside prose",
			"see https://www.canon.test/b\n" + ser("u", canonA)},
		// The body that separates the two byte-matcher passes, which the test's
		// own comment used to record as unreachable: with RewriteText dropped,
		// SweepBytes runs with value=true and absorbs the trailing root label.
		// The newline is load-bearing.
		{"a trailing root label after a newline", "x\nhttps://www.canon.test."},
		{"nothing to do", `<p>no origins here</p>`},
	}
	types := []string{
		"text/html", "application/xhtml+xml",
		"text/plain", "text/plain; charset=utf-8", "TEXT/PLAIN",
		"text/xml", "application/xml", "image/svg+xml",
		"application/rss+xml", "application/atom+xml", "application/vnd.foo+xml",
		"application/json", "text/json", "application/ld+json",
		"text/markdown", "text/calendar", "application/vnd.foo.xml",
		"text/css", "application/javascript", "application/octet-stream", "",
	}

	for _, ct := range types {
		for _, b := range bodies {
			t.Run(ct+"/"+b.name, func(t *testing.T) {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if ct != "" {
						w.Header().Set("Content-Type", ct)
					}
					_, _ = w.Write([]byte(b.body))
				}))
				defer up.Close()
				target, err := url.Parse(up.URL)
				if err != nil {
					t.Fatal(err)
				}
				front := httptest.NewServer((&proxy.Proxy{
					Upstream: target, Map: m, Stats: rewrite.NewStats(false),
				}).Handler())
				defer front.Close()

				req, err := http.NewRequest("GET", front.URL+"/", nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Host = "v.ddev.site"
				res, err := (&http.Client{}).Do(req)
				if err != nil {
					t.Fatal(err)
				}
				served, _ := io.ReadAll(res.Body)
				res.Body.Close()

				// The type the *response* carried, not the one requested: Go's
				// server sniffs when the handler sets none, and the real diff
				// tool reads it off the response for the same reason.
				scored, err := applyLikeTheProxy(m.Forward(), []byte(b.body),
					res.Header.Get("Content-Type"), rewrite.NewStats(false))
				if err != nil {
					t.Fatal(err)
				}
				if string(scored) != string(served) {
					t.Errorf("the scorer and the proxy disagree about this body:\n"+
						" in     %q\n proxy  %q\n scorer %q", b.body, served, scored)
				}
			})
		}
	}
}

const canonA = "https://www.canon.test/a"

// ser builds `a:1:{s:K:"key";s:N:"val";}` with correct lengths.
func ser(key, val string) string { return serRaw(key, val) }

func serRaw(key, val string) string {
	return fmt.Sprintf(`a:1:{s:%d:%q;s:%d:%q;}`, len(key), key, len(val), val)
}

// serPct is ser, percent-encoded the way a form sends it.
func serPct(key, val string) string {
	return url.QueryEscape(serRaw(key, val))
}
