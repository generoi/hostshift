package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

const (
	canonical = "https://www.herrfors.fi"
	variant   = "https://wt-a--herrfors.ddev.site"
)

func testMatcher(t *testing.T) *origin.Matcher {
	t.Helper()
	m, err := origin.NewMatcher([]origin.Pair{{
		Name:      "main",
		Canonical: origin.MustParse(canonical),
		Variant:   origin.MustParse(variant),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	return m
}

// serve stands up an upstream that returns body with the given headers, fronted
// by a hostshift proxy, and returns the response.
func serve(t *testing.T, body []byte, hdr map[string]string, dryRun bool) (*http.Response, []byte, *rewrite.Stats, *http.Request) {
	t.Helper()
	var seen *http.Request
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(r.Context())
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.Write(body)
	}))
	t.Cleanup(up.Close)
	target, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	st := rewrite.NewStats(false)
	p := &Proxy{
		Upstream:  target,
		Matcher:   testMatcher(t),
		Stats:     st,
		DryRun:    dryRun,
		Canonical: "www.herrfors.fi",
	}
	front := httptest.NewServer(p.Handler())
	t.Cleanup(front.Close)

	res, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res, got, st, seen
}

func corpus(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("../../spike/corpus/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("corpus not found: %v", err)
	}
	return files
}

// TestFilterMatchesProxyBytes is acceptance test 27: `hostshift rewrite` as a
// filter must produce the same bytes as the proxy path, over every real page in
// the corpus. Both go through rewrite.NewHTML, and this is what asserts they
// have not drifted apart — the filter is only a useful test harness if it is
// genuinely the same engine.
func TestFilterMatchesProxyBytes(t *testing.T) {
	m := testMatcher(t)
	for _, f := range corpus(t) {
		in, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}

		// filter path
		filtered, err := io.ReadAll(rewrite.NewHTML(bytes.NewReader(in), m, nil, rewrite.Options{}))
		if err != nil {
			t.Fatal(err)
		}

		// proxy path
		_, proxied, _, _ := serve(t, in, map[string]string{"Content-Type": "text/html; charset=UTF-8"}, false)

		if !bytes.Equal(filtered, proxied) {
			t.Errorf("%s: filter and proxy disagree (%d vs %d bytes)", f, len(filtered), len(proxied))
			continue
		}
		if bytes.Contains(proxied, []byte(canonical+"/")) {
			t.Errorf("%s: a canonical origin survived into the proxied body", f)
		}
	}
}

// TestNoStaleValidators is acceptance test 15. A stale Content-Length truncates
// or hangs the response; a surviving ETag lets a conditional request return 304
// and the browser serve a cached canonical-bearing body, which defeats test 28
// silently.
func TestNoStaleValidators(t *testing.T) {
	body := []byte(`<a href="` + canonical + `/x">t</a>`)
	res, got, _, _ := serve(t, body, map[string]string{
		"Content-Type":  "text/html",
		"ETag":          `"abc123"`,
		"Last-Modified": "Wed, 27 Aug 2026 10:00:00 GMT",
		"Accept-Ranges": "bytes",
	}, false)

	if v := res.Header.Get("Content-Length"); v != "" {
		t.Errorf("Content-Length survived: %q", v)
	}
	if v := res.Header.Get("ETag"); v != "" {
		t.Errorf("ETag survived: %q", v)
	}
	if v := res.Header.Get("Last-Modified"); v != "" {
		t.Errorf("Last-Modified survived: %q", v)
	}
	if v := res.Header.Get("Accept-Ranges"); v != "" {
		t.Errorf("Accept-Ranges survived: %q", v)
	}
	if len(got) == len(body) {
		t.Errorf("body was not rewritten: %q", got)
	}
}

// TestRequestDirection covers the PLAN §5.7 mechanics that are silent failures:
// the canonical Host must survive SetURL, X-Forwarded-Proto must be https
// despite SetXForwarded having just written http, and X-Forwarded-Port must not
// be forwarded at all.
func TestRequestDirection(t *testing.T) {
	_, _, _, seen := serve(t, []byte("<p>x</p>"), map[string]string{"Content-Type": "text/html"}, false)
	if seen == nil {
		t.Fatal("upstream saw no request")
	}
	if seen.Host != "www.herrfors.fi" {
		t.Errorf("upstream saw Host %q, want the canonical host — SetURL clears Out.Host and it must be reassigned after", seen.Host)
	}
	if got := seen.Header.Get("X-Forwarded-Proto"); got != "https" {
		t.Errorf("X-Forwarded-Proto is %q, want https — SetXForwarded writes http and must be overridden after it", got)
	}
	if got := seen.Header.Get("X-Forwarded-Port"); got != "" {
		t.Errorf("X-Forwarded-Port was forwarded as %q; it must be deleted (PLAN §2.3)", got)
	}
	if got := seen.Header.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("Accept-Encoding upstream is %q, want identity", got)
	}
}

// TestNonRewritableNeverEntersARewriter is acceptance test 25, and test 12 for
// the binary case: content outside the rewritable set streams through
// byte-identical, proven by a per-surface counter of zero.
func TestNonRewritableNeverEntersARewriter(t *testing.T) {
	// A PNG header followed by bytes that happen to spell the canonical origin.
	binary := append([]byte("\x89PNG\r\n\x1a\n"), []byte(canonical+"/x")...)

	for _, ct := range []string{"image/png", "text/css", "application/javascript", "application/json"} {
		res, got, st, _ := serve(t, binary, map[string]string{"Content-Type": ct}, false)
		if !bytes.Equal(got, binary) {
			t.Errorf("%s: body was not byte-identical", ct)
		}
		if n := st.Rewrites(rewrite.SurfaceHTMLAttr); n != 0 {
			t.Errorf("%s: html-attr counter is %d, want 0 — it entered a rewriter", ct, n)
		}
		// An unmodified response keeps its validators.
		if res.Header.Get("Content-Length") == "" && res.ContentLength != -1 {
			t.Errorf("%s: length handling changed on an unmodified response", ct)
		}
	}
}

// TestHeaderOriginsRewritten covers the Tier 1 header surface: Location is the
// login redirect of test 1, and CSP is test 23.
func TestHeaderOriginsRewritten(t *testing.T) {
	res, _, _, _ := serve(t, []byte("x"), map[string]string{
		"Content-Type":            "text/plain",
		"Location":                canonical + "/wp-admin/",
		"Content-Security-Policy": "default-src 'self' " + canonical,
		"Link":                    "<" + canonical + "/wp-json/>; rel=\"https://api.w.org/\"",
	}, false)

	for _, h := range []string{"Location", "Content-Security-Policy", "Link"} {
		v := res.Header.Get(h)
		if bytes.Contains([]byte(v), []byte(canonical)) {
			t.Errorf("%s still carries the canonical origin: %q", h, v)
		}
		if !bytes.Contains([]byte(v), []byte(variant)) {
			t.Errorf("%s was not rewritten to the variant: %q", h, v)
		}
	}
	// The rel= URI is a third-party host and must be untouched (test 8).
	if !bytes.Contains([]byte(res.Header.Get("Link")), []byte("https://api.w.org/")) {
		t.Errorf("Link lost its third-party rel URI: %q", res.Header.Get("Link"))
	}
}

// TestDryRunServesUnmodified: --dry-run is safe to point at a live canonical
// checkout (PLAN §5.8).
func TestDryRunServesUnmodified(t *testing.T) {
	body := []byte(`<a href="` + canonical + `/x">t</a>`)
	res, got, st, _ := serve(t, body, map[string]string{
		"Content-Type": "text/html",
		"ETag":         `"abc123"`,
		"Location":     canonical + "/y",
	}, true)

	if !bytes.Equal(got, body) {
		t.Errorf("--dry-run changed the body:\n got %q\nwant %q", got, body)
	}
	if v := res.Header.Get("Location"); v != canonical+"/y" {
		t.Errorf("--dry-run changed Location to %q", v)
	}
	if v := res.Header.Get("ETag"); v == "" {
		t.Error("--dry-run dropped the ETag; it must change nothing")
	}
	if st.Total() == 0 {
		t.Error("--dry-run counted no rewrites; it must still report what it would have done")
	}
}

// TestUpstreamFailureSurfaced is acceptance test 14: a connection failure
// becomes a 502 the developer can see, not a silent empty page.
func TestUpstreamFailureSurfaced(t *testing.T) {
	// Port 1 on loopback: nothing listens there.
	target, _ := url.Parse("http://127.0.0.1:1")
	p := &Proxy{
		Upstream:  target,
		Matcher:   testMatcher(t),
		Stats:     rewrite.NewStats(false),
		Canonical: "www.herrfors.fi",
	}
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	res, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want 502", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !bytes.Contains(body, []byte("hostshift")) {
		t.Errorf("the error body does not identify hostshift: %q", body)
	}
}

// TestIdentityMapThroughProxy is test 24 on the proxy path, closing the loop
// with the filter-side assertion in internal/rewrite.
func TestIdentityMapThroughProxy(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{{
		Name: "main", Canonical: origin.MustParse(canonical), Variant: origin.MustParse(canonical),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range corpus(t) {
		in, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Write(in)
		}))
		target, _ := url.Parse(up.URL)
		p := &Proxy{Upstream: target, Matcher: m, Stats: rewrite.NewStats(false), Canonical: "www.herrfors.fi"}
		front := httptest.NewServer(p.Handler())

		res, err := http.Get(front.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(res.Body)
		res.Body.Close()
		front.Close()
		up.Close()

		if !bytes.Equal(in, got) {
			t.Errorf("%s: identity map through the proxy changed the bytes (%d -> %d)", f, len(in), len(got))
		}
	}
}
