package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
