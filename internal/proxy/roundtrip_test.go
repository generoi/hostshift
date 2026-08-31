package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// The response manufactures obfuscated *variants*, and the request direction has
// to be able to reverse them.
//
// Only the matched host's byte range is spliced, so `https:\\www.example.fi/a`
// reaches the browser as `https:\\wt-a--example.ddev.site/a`. The byte matcher's
// prefilter needs `//`, `\/` or `%2F` and that string has none, so a form post
// carrying it back went upstream unreversed — writing a worktree-local hostname
// into the database §4.3 says stays byte-identical to production and is shared
// by canonical, every worktree and CI.
func TestObfuscatedVariantsAreReversed(t *testing.T) {
	mp, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--example.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	rev := mp.Reverse()

	for _, c := range []struct{ name, in string }{
		{"backslashes", `https:\\wt-a--example.ddev.site/a`},
		{"three slashes", `https:///wt-a--example.ddev.site/a`},
		{"a differing scheme", `http:wt-a--example.ddev.site/a`},
		{"userinfo", `https://u@wt-a--example.ddev.site/a`},
		{"scheme relative backslashes", `\\wt-a--example.ddev.site/a`},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := rewrite.HostLeaks(rev, []byte(c.in), true)
			if strings.Contains(string(out), "wt-a--example.ddev.site") {
				t.Errorf("a variant hostname would be written upstream:\n%s", out)
			}
			if !strings.Contains(string(out), "www.example.fi") {
				t.Errorf("not reversed to the canonical:\n%s", out)
			}
		})
	}
}

// A Location is followed by the browser through the URL parser, so an
// obfuscated or folded host in one is a navigation to production.
func TestObfuscatedLocationIsRewritten(t *testing.T) {
	mp, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://www.example.fi"),
		Variant:   origin.MustParse("https://wt-a--example.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", `https:\\www.example.fi/next`)
		w.WriteHeader(http.StatusFound)
	}))
	defer up.Close()

	upURL, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Map: mp, Upstream: upURL, Stats: rewrite.NewStats(false)}
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/x", nil)
	req.Host = "wt-a--example.ddev.site"
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if loc := resp.Header.Get("Location"); strings.Contains(loc, "www.example.fi") {
		t.Errorf("a production origin reached the browser in Location: %s", loc)
	}
}

// The response-side content-type gate, which had two whole families outside it.
//
// wp-admin/async-upload.php sets `text/plain` before wp_send_json can set
// application/json, so every media upload handed the browser a canonical `url`
// while the listing endpoint beside it was rewritten correctly — and --explain
// printed nothing at all for it, not even a skip. Feeds and sitemaps are the
// same shape: full of absolute URLs, rendered by a browser, dereferenced by a
// reader.
func TestResponseContentTypes(t *testing.T) {
	mp, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://acme.ddev.site"),
		Variant:   origin.MustParse("https://wt-a--acme.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	const body = `{"url":"https:\/\/acme.ddev.site\/wp-content\/uploads\/px.png"}`

	for _, c := range []struct {
		ct      string
		rewrite bool
	}{
		{"text/html; charset=UTF-8", true},
		{"application/json", true},
		// The upload endpoint.
		{"text/plain; charset=UTF-8", true},
		{"application/xml", true},
		{"application/rss+xml", true},
		{"application/atom+xml", true},
		{"image/svg+xml", true},
		// §5.2 Tier 2, measured: 88 CSS and 185 JS files in the fleet's themes,
		// zero absolute URLs. Deliberately outside the set.
		{"text/css", false},
		{"application/javascript", false},
		{"image/png", false},
	} {
		t.Run(c.ct, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", c.ct)
				w.Write([]byte(body))
			}))
			defer up.Close()
			upURL, err := url.Parse(up.URL)
			if err != nil {
				t.Fatal(err)
			}
			p := &Proxy{Map: mp, Upstream: upURL, Stats: rewrite.NewStats(false)}
			srv := httptest.NewServer(p.Handler())
			defer srv.Close()

			req, _ := http.NewRequest("GET", srv.URL+"/x", nil)
			req.Host = "wt-a--acme.ddev.site"
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, _ := io.ReadAll(resp.Body)

			if c.rewrite && !strings.Contains(string(got), "wt-a--acme.ddev.site") {
				t.Errorf("a canonical origin reached the browser as %s:\n%s", c.ct, got)
			}
			if !c.rewrite && string(got) != body {
				t.Errorf("%s is outside the rewritable set but changed:\n%s", c.ct, got)
			}
		})
	}
}

// A download is a file the developer saves, not a page the browser renders, so
// the hostnames in it outlive this machine. `Tools → Export` returns text/xml
// with Content-Disposition: attachment, and every <link>, <guid> and
// <wp:base_site_url> in the WXR came back naming a worktree — a file that looks
// fine, imports fine, and points at a hostname that exists on one machine.
func TestDownloadsAreNotRewritten(t *testing.T) {
	mp, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://acme.ddev.site"),
		Variant:   origin.MustParse("https://wt-a--acme.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	const body = `<rss><link>https://acme.ddev.site/post</link></rss>`

	for _, c := range []struct {
		name, disposition string
		rewrite           bool
	}{
		{"an export", "attachment; filename=\"acme.WordPress.xml\"", false},
		{"a bare attachment", "attachment", false},
		{"an inline body", "inline", true},
		{"no disposition at all", "", true},
	} {
		// Every content type, not just the one where the arm happened to be
		// reachable. Pinning text/xml here is why the arm sat third in the switch
		// — after the HTML and JSON arms, which it could therefore never reach —
		// while ACF and Elementor exports, both application/json with
		// Content-Disposition: attachment, went on being rewritten.
		for _, ct := range []string{
			"text/xml; charset=UTF-8", "application/json", "text/html", "text/plain",
		} {
			t.Run(c.name+" as "+ct, func(t *testing.T) {
				runDownload(t, mp, body, c.disposition, ct, c.rewrite)
			})
		}
	}
}

func runDownload(t *testing.T, mp *origin.Map, body, disposition, ct string, want bool) {
	t.Helper()
	{
		{
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", ct)
				c := struct{ disposition string }{disposition}
				_ = c
				if disposition != "" {
					w.Header().Set("Content-Disposition", disposition)
				}
				w.Write([]byte(body))
			}))
			defer up.Close()
			upURL, err := url.Parse(up.URL)
			if err != nil {
				t.Fatal(err)
			}
			p := &Proxy{Map: mp, Upstream: upURL, Stats: rewrite.NewStats(false)}
			srv := httptest.NewServer(p.Handler())
			defer srv.Close()

			req, _ := http.NewRequest("GET", srv.URL+"/export", nil)
			req.Host = "wt-a--acme.ddev.site"
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, _ := io.ReadAll(resp.Body)

			if want && !strings.Contains(string(got), "wt-a--acme.ddev.site") {
				t.Errorf("not rewritten as %s:\n%s", ct, got)
			}
			if !want && string(got) != body {
				t.Errorf("a download was rewritten as %s, so it names a hostname that exists on one machine:\n%s", ct, got)
			}
		}
	}
}

// Character references are decoded where the *consuming* parser decodes them,
// not where HTML does.
//
// decodeURLRefs runs on HTML attribute values only, justified by "inside
// <script> and <style> the browser does not decode references" — true of HTML,
// false of XML. In XHTML the XML parser decodes them there (which is why XHTML
// scripts need CDATA), and in the XML family it decodes them everywhere. Both
// were confirmed dereferenced by a real browser. text/plain is the other way:
// nothing parses references in it, so leaving them alone is correct.
func TestReferencesDecodeWhereTheParserDoes(t *testing.T) {
	mp, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://acme.ddev.site"),
		Variant:   origin.MustParse("https://wt-a--acme.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, ct, body string
		rewrite        bool
	}{
		{"an SVG href", "image/svg+xml",
			`<svg><image href="https:&#47;&#47;acme.ddev.site/p.png"/></svg>`, true},
		{"a feed link", "application/rss+xml",
			`<rss><link>https:&#47;&#47;acme.ddev.site/p</link></rss>`, true},
		{"a sitemap loc", "application/xml",
			`<urlset><loc>https:&#47;&#47;acme.ddev.site/p</loc></urlset>`, true},
		{"an XHTML script", "application/xhtml+xml",
			`<html><script>var u="https:&#47;&#47;acme.ddev.site/s";</script></html>`, true},
		{"an XHTML style", "application/xhtml+xml",
			`<html><style>a{background:url(https:&#47;&#47;acme.ddev.site/c)}</style></html>`, true},
		// An HTML parser does not decode inside script or style, so a reference
		// there is not a URL and must not be touched.
		{"an HTML script", "text/html",
			`<html><script>var u="https:&#47;&#47;acme.ddev.site/s";</script></html>`, false},
		// Nothing parses references in plain text.
		{"plain text", "text/plain", `see https:&#47;&#47;acme.ddev.site/p`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", c.ct)
				w.Write([]byte(c.body))
			}))
			defer up.Close()
			upURL, err := url.Parse(up.URL)
			if err != nil {
				t.Fatal(err)
			}
			p := &Proxy{Map: mp, Upstream: upURL, Stats: rewrite.NewStats(false)}
			srv := httptest.NewServer(p.Handler())
			defer srv.Close()

			req, _ := http.NewRequest("GET", srv.URL+"/x", nil)
			req.Host = "wt-a--acme.ddev.site"
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, _ := io.ReadAll(resp.Body)

			if c.rewrite && !strings.Contains(string(got), "wt-a--acme.ddev.site") {
				t.Errorf("a production origin the parser dereferences was left:\n%s", got)
			}
			if !c.rewrite && string(got) != c.body {
				t.Errorf("a reference no parser decodes here was rewritten:\n%s", got)
			}
			// Whatever happened, the escapes themselves survive: the splice
			// replaces the host's byte range and never re-serialises the value.
			if !strings.Contains(string(got), "&#47;&#47;") {
				t.Errorf("the references were re-serialised:\n%s", got)
			}
		})
	}
}

// wp-admin/async-upload.php sets `text/plain` before wp_send_json can set
// application/json — the endpoint this arm was added for — so the body is
// exactly what wp_json_encode produced, `\uXXXX` escapes included. PLAN M4 lists
// that spelling as a test-28 leak because wp_json_encode does not pass
// JSON_UNESCAPED_UNICODE. The raw-UTF-8 IDN host was caught; the escaped form
// PHP actually emits was not.
func TestEscapedIDNUnderATextLabel(t *testing.T) {
	mp, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://xn--hmeen-gra.fi"),
		Variant:   origin.MustParse("https://wt-a--hameen.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	const body = `{"url":"https:\/\/h\u00e4meen.fi\/a.png"}`

	for _, ct := range []string{"application/json", "text/plain; charset=UTF-8"} {
		t.Run(ct, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", ct)
				w.Write([]byte(body))
			}))
			defer up.Close()
			upURL, err := url.Parse(up.URL)
			if err != nil {
				t.Fatal(err)
			}
			p := &Proxy{Map: mp, Upstream: upURL, Stats: rewrite.NewStats(false)}
			srv := httptest.NewServer(p.Handler())
			defer srv.Close()

			req, _ := http.NewRequest("GET", srv.URL+"/x", nil)
			req.Host = "wt-a--hameen.ddev.site"
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(got), "wt-a--hameen.ddev.site") {
				t.Errorf("an escaped IDN canonical reached the browser as %s:\n%s", ct, got)
			}
		})
	}
}

// A serialized blob in the query string, the Referer, and a Location keeps a
// length that describes its data.
//
// Every body arm has gone through RepairSerialized since round 22. The request
// line and the headers never did — they called Matcher.Rewrite directly — so a
// blob carried in a link hostshift itself served with its lengths repaired came
// back through the query string with every length stale, and PHP refused it.
//
// Nothing scored it. `hostshift diff` does not look at requests at all, and
// compares headers only for leaks; the integration suite's redirect_to
// assertion carries a bare URL, which has no length prefix to get wrong.
func TestASerializedBlobSurvivesTheRequestLineAndHeaders(t *testing.T) {
	canon, variant := "https://www.example.fi", "https://wt-a--example.ddev.site"
	mp, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse(canon),
		Variant:   origin.MustParse(variant),
	}})
	if err != nil {
		t.Fatal(err)
	}
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	// Two levels, because one level's length is re-emitted by the outer walk
	// even when the nested one is missed.
	blob := func(host string) string {
		inner := `a:1:{` + str("href") + str(host+"/sv/produkter/") + `}`
		return `a:1:{` + str("note") + str("Obs: "+inner) + `}`
	}

	var gotQuery, gotReferer string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotReferer = r.Header.Get("Referer")
		w.Header().Set("Location", "/landing.php?state="+url.QueryEscape(blob(canon)))
		w.WriteHeader(http.StatusFound)
	}))
	defer up.Close()
	upURL, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Map: mp, Upstream: upURL, Stats: rewrite.NewStats(false)}
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	// The browser sends the variant spelling, which is what it was served.
	req, _ := http.NewRequest("GET", srv.URL+"/c.php?blob="+url.QueryEscape(blob(variant)), nil)
	req.Host = "wt-a--example.ddev.site"
	req.Header.Set("Referer", variant+"/p?state="+url.QueryEscape(blob(variant)))
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// What the application received, and what the browser is sent.
	for name, got := range map[string]string{
		"query string":    gotQuery,
		"Referer":         gotReferer,
		"Location header": resp.Header.Get("Location"),
	} {
		i := strings.Index(got, "=")
		if i < 0 {
			t.Fatalf("%s: no value in %q", name, got)
		}
		dec, err := url.QueryUnescape(got[i+1:])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := blob(canon)
		if name == "Location header" {
			want = blob(variant)
		}
		if dec != want {
			t.Errorf("%s:\n got  %s\n want %s", name, dec, want)
		}
		if n := rewrite.BrokenSerialized([]byte(dec)); n != 0 {
			t.Errorf("%s: %d value(s) with a length that does not describe the data:\n %s",
				name, n, dec)
		}
	}
}

// A Set-Cookie has its Domain= mapped and nothing else touched.
//
// PLAN's Tier 1 table scopes Set-Cookie to `Domain=`; PLAN's test 28 said
// "every URL-valued position — … header values". Those disagree, and the audit
// that found the disagreement found a production origin sitting readable in a
// non-HttpOnly cookie value on a served page.
//
// The table wins, and the reason is worth holding in place: a session cookie is
// signed over its contents, so rewriting a host inside `wordpress_logged_in_…`
// invalidates the signature and logs the developer out. This asserts both
// halves — Domain= is handled, the value is not — so the boundary stays a
// decision someone made rather than something nobody noticed.
func TestSetCookieHasItsDomainMappedAndItsValueLeftAlone(t *testing.T) {
	canon, variant := "https://www.example.fi", "https://wt-a--example.ddev.site"
	mp, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse(canon),
		Variant:   origin.MustParse(variant),
	}})
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "hs_pref="+url.QueryEscape(canon+"/sv/")+"; Path=/")
		w.Header().Add("Set-Cookie", "sess=abc; Domain=.www.example.fi; Path=/; HttpOnly")
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	upURL, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Map: mp, Upstream: upURL, Stats: rewrite.NewStats(false)}
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Host = "wt-a--example.ddev.site"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	var value, domain string
	for _, c := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(c, "hs_pref=") {
			value = c
		}
		if strings.HasPrefix(c, "sess=") {
			domain = c
		}
	}
	// The Domain= is not left naming a host the browser is not on.
	if strings.Contains(domain, "www.example.fi") {
		t.Errorf("Domain= still names the canonical host: %s", domain)
	}
	// And the value is untouched, signature and all.
	if !strings.Contains(value, url.QueryEscape(canon+"/sv/")) {
		t.Errorf("the cookie value was rewritten, which breaks a signed cookie: %s", value)
	}
}
