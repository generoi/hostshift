package proxy

import (
	"fmt"
	gohtml "html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// occupiesItsField (serialized.go) declines to re-emit a length for a
// serialized value embedded in a larger document, and its own comment states
// the justification:
//
//	"What that costs: a serialized value embedded in a larger document — inside
//	 CDATA in a WXR export, or mid-paragraph — no longer has its length
//	 re-emitted. It is still rewritten, and the response direction declines for
//	 the same reason, so the two directions stay consistent and the round trip
//	 is exact."
//
// The round trip is not exact. The forward pass replaces the whole origin with
// the *variant's declared* origin, scheme included — matcher.go matches both
// schemes for every canonical host on purpose, because "the fleet's databases
// carry http:// forms of hosts declared as https (M0 measured
// nat.acmecorp.ddev.site appearing 165 times over http and zero over https)".
// So an `http://` occurrence of a canonical host goes out as `https://variant`
// and comes back as `https://canonical` — one byte longer than it left.
//
// Where the length was re-emitted that is merely a scheme upgrade. Where it was
// declined, the stale length that the forward pass left behind no longer
// describes the data on the way back either, and the value written upstream is
// one PHP refuses: `false` where post_content or an options array should be, in
// the shared production database PLAN §4.3 says stays byte-identical to
// production. Nothing scores it — `hostshift diff` never looks at a request.
//
// The https case in this test is the control: identical fixture, identical
// decline, and it round-trips byte for byte, which is what the comment above
// promises for both.
func TestADeclinedValueDoesNotSurviveASchemeMismatch(t *testing.T) {
	if _, err := exec.LookPath("php"); err != nil {
		t.Skip("php is the oracle for this test")
	}
	mp, err := origin.NewMap([]origin.Site{{
		Name: "main",
		// The two hosts differ in length, so a stale length is distinguishable
		// from a correct one.
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--example.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	for _, scheme := range []string{"https", "http"} {
		t.Run(scheme, func(t *testing.T) {
			// A serialized shortcode attribute mid-paragraph: exactly the shape
			// occupiesItsField's comment names as the cost it accepts. The
			// length is computed, never hardcoded.
			u := scheme + "://www.example.fi/x"
			blob := fmt.Sprintf(`a:1:{s:3:"url";s:%d:"%s";}`, len(u), u)
			if !phpUnserializes(t, blob) {
				t.Fatalf("the fixture itself does not parse: %s", blob)
			}
			content := "<p>intro</p>[gallery data=" + blob + "]<p>outro</p>"

			var stored string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					b, _ := io.ReadAll(r.Body)
					vs, err := url.ParseQuery(string(b))
					if err != nil {
						t.Errorf("upstream body is not a form: %v", err)
					}
					stored = vs.Get("content")
					w.WriteHeader(http.StatusNoContent)
					return
				}
				// The classic editor's post_content box. htmlspecialchars, so
				// no raw quote sits inside the element.
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				io.WriteString(w, `<form method="post"><textarea name="content">`+
					htmlSpecialChars(content)+`</textarea></form>`)
			}))
			defer up.Close()
			upURL, err := url.Parse(up.URL)
			if err != nil {
				t.Fatal(err)
			}
			p := &Proxy{Map: mp, Upstream: upURL, Stats: rewrite.NewStats(false)}
			srv := httptest.NewServer(p.Handler())
			defer srv.Close()

			req, _ := http.NewRequest("GET", srv.URL+"/wp-admin/post.php", nil)
			req.Host = "wt-a--example.ddev.site"
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			page, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			// What the browser hands the form on submit: the textarea's value,
			// with character references decoded.
			const open = `<textarea name="content">`
			i := strings.Index(string(page), open)
			j := strings.Index(string(page), "</textarea>")
			if i < 0 || j < i {
				t.Fatalf("no textarea in the served page:\n%s", page)
			}
			edited := gohtml.UnescapeString(string(page)[i+len(open) : j])

			body := "content=" + url.QueryEscape(edited)
			req2, _ := http.NewRequest("POST", srv.URL+"/wp-admin/post.php", strings.NewReader(body))
			req2.Host = "wt-a--example.ddev.site"
			req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp2, err := http.DefaultClient.Do(req2)
			if err != nil {
				t.Fatal(err)
			}
			resp2.Body.Close()

			k := strings.Index(stored, "a:1:")
			l := strings.LastIndex(stored, ";}")
			if k < 0 || l <= k {
				t.Fatalf("no serialized value reached upstream:\n%s", stored)
			}
			back := stored[k : l+2]
			ok := phpUnserializes(t, back)
			// https round-trips; http does not, and that asymmetry is the whole
			// finding. Both are asserted, so the test fails if either changes.
			//
			// If the http case starts passing, one of two things happened: the
			// decline was removed, or the substitution was made scheme-symmetric.
			// The first reintroduces the destroyed wp_options row — see
			// TestAnOrdinaryCustomCSSOptionSurvivesARoundTrip and
			// TestTheResidueRulesBlindSpotComesHome, which both fail with the
			// field check off. Check those before celebrating, and update the
			// comment in serialized.go that this pins.
			if scheme == "https" && !ok {
				t.Errorf("the byte-symmetric case stopped round-tripping:\n"+
					" served %s\n stored %s", blob, back)
			}
			if scheme == "http" && ok {
				t.Errorf("the scheme-mismatched case now round-trips — see the comment "+
					"above; if this is a real fix, serialized.go's stated cost is stale:\n"+
					" served %s\n stored %s", blob, back)
			}
		})
	}
}

// phpUnserializes asks PHP itself whether s is a value it will accept.
func phpUnserializes(t *testing.T, s string) bool {
	t.Helper()
	cmd := exec.Command("php", "-r",
		`$v = unserialize(stream_get_contents(STDIN)); if ($v === false) { exit(1); } echo json_encode($v);`)
	cmd.Stdin = strings.NewReader(s)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	t.Logf("php unserialize(%s) = %s", s, out)
	return true
}

// htmlSpecialChars is what WordPress renders a textarea's contents through.
func htmlSpecialChars(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '&':
			b.WriteString("&amp;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&#039;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
