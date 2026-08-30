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
