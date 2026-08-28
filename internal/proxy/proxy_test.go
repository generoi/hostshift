package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

const (
	canonical   = "https://www.herrfors.fi"
	variant     = "https://wt-a--herrfors.ddev.site"
	variantHost = "wt-a--herrfors.ddev.site"

	natCanonical   = "https://www.herrforsnat.fi"
	natVariant     = "https://wt-a--nat.herrfors.ddev.site"
	natVariantHost = "wt-a--nat.herrfors.ddev.site"
)

// herrforsMap is the two-blog map, with the ddev host listed as an alias so a
// residual @ddev URL is corrected too (PLAN §4.2).
func herrforsMap(t *testing.T) *origin.Map {
	t.Helper()
	m, err := origin.NewMap([]origin.Site{
		{
			Name:      "main",
			Canonical: origin.MustParse(canonical),
			Aliases:   []origin.Origin{origin.MustParse("https://herrfors.ddev.site")},
			Variant:   origin.MustParse(variant),
		},
		{
			Name:      "nat",
			Canonical: origin.MustParse(natCanonical),
			Aliases:   []origin.Origin{origin.MustParse("https://nat.herrfors.ddev.site")},
			Variant:   origin.MustParse(natVariant),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

type harness struct {
	front *httptest.Server
	stats *rewrite.Stats
	seen  *http.Request
	proxy *Proxy
}

// upstreamFunc is a handler standing in for the WordPress container.
type upstreamFunc func(w http.ResponseWriter, r *http.Request)

func newHarness(t *testing.T, m *origin.Map, h upstreamFunc, tune ...func(*Proxy)) *harness {
	t.Helper()
	hs := &harness{stats: rewrite.NewStats(false)}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		clone := r.Clone(r.Context())
		clone.Body = io.NopCloser(bytes.NewReader(body))
		hs.seen = clone
		r.Body = io.NopCloser(bytes.NewReader(body))
		h(w, r)
	}))
	t.Cleanup(up.Close)
	target, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	hs.proxy = &Proxy{Upstream: target, Map: m, Stats: hs.stats}
	for _, f := range tune {
		f(hs.proxy)
	}
	hs.front = httptest.NewServer(hs.proxy.Handler())
	t.Cleanup(hs.front.Close)
	return hs
}

// get issues a request to the proxy as if the browser were on host.
func (h *harness) get(t *testing.T, host, path string) (*http.Response, []byte) {
	t.Helper()
	return h.do(t, "GET", host, path, "", nil)
}

func (h *harness) do(t *testing.T, method, host, path, ctype string, body []byte) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, h.front.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	// Do not follow redirects: several tests assert on the redirect itself.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res, got
}

func html(body string) upstreamFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		io.WriteString(w, body)
	}
}

func corpus(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("../../spike/corpus/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("corpus not found: %v", err)
	}
	return files
}

// ---------------------------------------------------------------------------
// request direction (PLAN §5.1)

// TestMultisiteInverseMapping is acceptance test 10: a request for nat.V must
// arrive upstream as the *sibling blog's* canonical host, not the network's main
// host, and its response URLs must come back as nat.V.
//
// ms-settings.php lowercases and strips :80/:443, then get_site_by_path()
// matches wp_blogs.domain exactly — so anything but the sibling's own host
// resolves to the wrong blog, silently.
func TestMultisiteInverseMapping(t *testing.T) {
	m := herrforsMap(t)
	h := newHarness(t, m, html(`<a href="`+natCanonical+`/x">nat</a>`))

	_, got := h.get(t, natVariantHost, "/")
	if h.seen.Host != "www.herrforsnat.fi" {
		t.Errorf("upstream saw Host %q, want the sibling blog's canonical host", h.seen.Host)
	}
	if !bytes.Contains(got, []byte(natVariant+"/x")) {
		t.Errorf("response did not come back on the sibling variant: %s", got)
	}

	// And blog 1 still resolves to blog 1.
	h2 := newHarness(t, m, html(`<a href="`+canonical+`/y">main</a>`))
	_, got2 := h2.get(t, variantHost, "/")
	if h2.seen.Host != "www.herrfors.fi" {
		t.Errorf("blog 1 upstream saw Host %q", h2.seen.Host)
	}
	if !bytes.Contains(got2, []byte(variant+"/y")) {
		t.Errorf("blog 1 response: %s", got2)
	}
}

// TestCrossBlogLink is acceptance test 10b: blog 1 linking to blog 2's canonical
// is rewritten to blog 2's *variant*, not blog 1's.
func TestCrossBlogLink(t *testing.T) {
	h := newHarness(t, herrforsMap(t), html(
		`<a href="`+canonical+`/a">one</a><a href="`+natCanonical+`/b">two</a>`))
	_, got := h.get(t, variantHost, "/")
	if !bytes.Contains(got, []byte(variant+"/a")) || !bytes.Contains(got, []byte(natVariant+"/b")) {
		t.Errorf("cross-blog link not mapped per blog: %s", got)
	}
}

// TestResidualEnvironmentURL is acceptance test 10c: a residual @ddev URL in a
// database is rewritten to the variant too, because aliases are in the
// canonical set.
func TestResidualEnvironmentURL(t *testing.T) {
	h := newHarness(t, herrforsMap(t), html(`<img src="https://herrfors.ddev.site/app/x.png">`))
	_, got := h.get(t, variantHost, "/")
	if !bytes.Contains(got, []byte(variant+"/app/x.png")) {
		t.Errorf("residual @ddev URL was not corrected: %s", got)
	}
}

// TestUnmappedHostIsRejected is acceptance test 16: never proxied, 421.
func TestUnmappedHostIsRejected(t *testing.T) {
	h := newHarness(t, herrforsMap(t), html("<p>x</p>"))
	res, body := h.get(t, "somewhere.else.example", "/")
	if res.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("status %d, want 421", res.StatusCode)
	}
	if h.seen != nil {
		t.Error("the request was proxied upstream despite an unmapped Host")
	}
	if !bytes.Contains(body, []byte("hostshift")) {
		t.Errorf("the 421 body does not explain itself: %s", body)
	}
}

// TestQueryAndRefererRewritten covers the request half of the login round trip
// (test 19). wp-login.php?redirect_to= is validated by wp_validate_redirect()
// against home_url()'s host, so a variant origin is silently discarded and login
// returns to the wrong place. Referer must be mapped for the same reason:
// wp_get_referer() is false throughout wp-admin without it.
func TestQueryAndRefererRewritten(t *testing.T) {
	h := newHarness(t, herrforsMap(t), html("<p>x</p>"))

	req, _ := http.NewRequest("GET", h.front.URL+
		"/wp-login.php?redirect_to="+url.QueryEscape(variant+"/wp-admin/")+"&plain="+variant+"/z", nil)
	req.Host = variantHost
	req.Header.Set("Referer", variant+"/previous")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	q := h.seen.URL.RawQuery
	if strings.Contains(q, variantHost) {
		t.Errorf("a variant origin survived into the upstream query: %q", q)
	}
	if !strings.Contains(q, url.QueryEscape(canonical+"/wp-admin/")) {
		t.Errorf("percent-encoded redirect_to was not mapped to canonical: %q", q)
	}
	if !strings.Contains(q, canonical+"/z") {
		t.Errorf("plain origin in the query was not mapped to canonical: %q", q)
	}
	if ref := h.seen.Header.Get("Referer"); ref != canonical+"/previous" {
		t.Errorf("Referer is %q, want the canonical origin", ref)
	}
}

// TestForceSSLAdminDoesNotLoop is acceptance test 20. The upstream stands in for
// wp-login.php: force_ssl_admin() && !is_ssl() redirects, and is_ssl() is true
// only when X-Forwarded-Proto says https.
func TestForceSSLAdminDoesNotLoop(t *testing.T) {
	h := newHarness(t, herrforsMap(t), func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Forwarded-Proto") != "https" {
			http.Redirect(w, r, canonical+r.URL.Path, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<p>login form</p>")
	})
	res, got := h.get(t, variantHost, "/wp-login.php")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 — the site is redirect-looping on is_ssl()", res.StatusCode)
	}
	if !bytes.Contains(got, []byte("login form")) {
		t.Errorf("body: %s", got)
	}
}

// TestRequestBodyRewritten is the request half of tests 30 and 31: a write
// carrying a variant URL must be stored canonical, or the clone is polluted and
// edit round trips break.
func TestRequestBodyRewritten(t *testing.T) {
	for _, c := range []struct{ name, ctype, body, want string }{
		{
			"form-urlencoded",
			"application/x-www-form-urlencoded",
			"content=" + url.QueryEscape(`<a href="`+variant+`/x">k</a>`) + "&post_id=1",
			url.QueryEscape(canonical + "/x"),
		},
		{
			"rest json",
			"application/json",
			`{"content":"<a href=\"` + variant + `/x\">k</a>"}`,
			canonical + `/x`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, herrforsMap(t), html("ok"))
			h.do(t, "POST", variantHost, "/wp-json/wp/v2/posts", c.ctype, []byte(c.body))

			got, _ := io.ReadAll(h.seen.Body)
			if strings.Contains(string(got), variantHost) {
				t.Errorf("a variant origin survived into the upstream body: %s", got)
			}
			if !strings.Contains(string(got), c.want) {
				t.Errorf("body was not mapped to canonical:\n got %s\nwant it to contain %s", got, c.want)
			}
			// http.Request.ContentLength drives the transport, not the header.
			if h.seen.ContentLength != int64(len(got)) {
				t.Errorf("ContentLength is %d but the body is %d bytes — the transport will error or truncate",
					h.seen.ContentLength, len(got))
			}
		})
	}
}

// TestMultipartFilePartsUntouched: only non-file text parts are rewritten, and
// everything else — boundaries, headers, file bytes — is byte-identical.
func TestMultipartFilePartsUntouched(t *testing.T) {
	const b = "----hsbound"
	filePart := "\x89PNG\r\n\x1a\n" + variant + "/not-a-url-here"
	body := "--" + b + "\r\n" +
		"Content-Disposition: form-data; name=\"content\"\r\n\r\n" +
		`<a href="` + variant + `/x">k</a>` + "\r\n" +
		"--" + b + "\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"a.png\"\r\n" +
		"Content-Type: image/png\r\n\r\n" +
		filePart + "\r\n" +
		"--" + b + "--\r\n"

	h := newHarness(t, herrforsMap(t), html("ok"))
	h.do(t, "POST", variantHost, "/wp-admin/async-upload.php", "multipart/form-data; boundary="+b, []byte(body))

	got, _ := io.ReadAll(h.seen.Body)
	s := string(got)
	if !strings.Contains(s, canonical+"/x") {
		t.Errorf("the text part was not rewritten:\n%s", s)
	}
	if !strings.Contains(s, filePart) {
		t.Errorf("the file part was modified; it must pass through byte-identical:\n%s", s)
	}
	if strings.Count(s, "--"+b) != strings.Count(body, "--"+b) {
		t.Errorf("boundaries changed: %d, want %d", strings.Count(s, "--"+b), strings.Count(body, "--"+b))
	}
}

// TestOversizeBodyPassesThrough: above the cap a body streams through untouched
// and the skip is logged (PLAN §5.8).
func TestOversizeBodyPassesThrough(t *testing.T) {
	body := []byte(strings.Repeat("x", 4096) + variant + "/x")
	h := newHarness(t, herrforsMap(t), html("ok"), func(p *Proxy) { p.MaxBody = 1024 })
	h.do(t, "POST", variantHost, "/x", "application/json", body)

	got, _ := io.ReadAll(h.seen.Body)
	if !bytes.Equal(got, body) {
		t.Errorf("an over-cap body was modified (%d in, %d out)", len(body), len(got))
	}
}

// ---------------------------------------------------------------------------
// response direction

// TestSelfRedirectGuard is acceptance test 32 (PLAN §4.4). The fleet's
// redirect-uploads.conf 302s a missing /app/uploads/ request to production;
// rewriting that Location sends the browser back to the request it just made.
func TestSelfRedirectGuard(t *testing.T) {
	const path = "/app/uploads/2025/07/x.jpg"
	uploads := func(w http.ResponseWriter, r *http.Request) {
		// Exactly what `rewrite ^ https://www.herrfors.fi$request_uri redirect`
		// emits for a file that is not on disk.
		http.Redirect(w, r, canonical+r.URL.Path, http.StatusFound)
	}

	t.Run("passes through, counted", func(t *testing.T) {
		h := newHarness(t, herrforsMap(t), uploads)
		res, _ := h.get(t, variantHost, path)
		if res.StatusCode != http.StatusFound {
			t.Fatalf("status %d, want 302", res.StatusCode)
		}
		if got := res.Header.Get("Location"); got != canonical+path {
			t.Errorf("Location is %q; rewriting it to the variant is the redirect loop", got)
		}
		if n := h.stats.Snapshot().Skips[origin.ReasonSelfRedirect]; n != 1 {
			t.Errorf("self-redirect count is %d, want 1 — the carve-out must be counted, not merely tolerated", n)
		}
	})

	t.Run("strict-origins returns 404 instead", func(t *testing.T) {
		h := newHarness(t, herrforsMap(t), uploads, func(p *Proxy) { p.StrictOrigins = true })
		res, body := h.get(t, variantHost, path)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("status %d, want 404 under --strict-origins", res.StatusCode)
		}
		if bytes.Contains(body, []byte(canonical)) {
			t.Errorf("--strict-origins let a canonical origin reach the browser: %s", body)
		}
	})

	t.Run("an ordinary redirect is still rewritten", func(t *testing.T) {
		// Test 1: a login redirect goes somewhere else, so the guard must not
		// catch it.
		h := newHarness(t, herrforsMap(t), func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, canonical+"/wp-admin/", http.StatusFound)
		})
		res, _ := h.get(t, variantHost, "/wp-login.php")
		if got := res.Header.Get("Location"); got != variant+"/wp-admin/" {
			t.Errorf("Location is %q, want the variant — the guard caught a redirect it should not have", got)
		}
	})
}

// TestSetCookieDomainDropped is acceptance test 2. ms_cookie_constants() defines
// COOKIE_DOMAIN from the network domain on any subdomain multisite that does not
// set it explicitly, so the cookie is discarded by the browser and login fails
// outright. Dropping the attribute is always safe.
func TestSetCookieDomainDropped(t *testing.T) {
	h := newHarness(t, herrforsMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "wordpress_logged_in=abc; Domain=.www.herrfors.fi; Path=/; HttpOnly")
		w.Header().Add("Set-Cookie", "third_party=1; Domain=.analytics.example; Path=/")
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "ok")
	})
	res, _ := h.get(t, variantHost, "/")

	cookies := res.Header.Values("Set-Cookie")
	if len(cookies) != 2 {
		t.Fatalf("got %d Set-Cookie headers, want 2", len(cookies))
	}
	if strings.Contains(cookies[0], "Domain=") {
		t.Errorf("the canonical Domain= attribute survived: %q", cookies[0])
	}
	if !strings.Contains(cookies[0], "HttpOnly") || !strings.Contains(cookies[0], "wordpress_logged_in=abc") {
		t.Errorf("dropping Domain= damaged the rest of the cookie: %q", cookies[0])
	}
	if !strings.Contains(cookies[1], "Domain=.analytics.example") {
		t.Errorf("a third-party cookie domain was touched: %q", cookies[1])
	}
}

// TestNoStaleValidators is acceptance test 15.
func TestNoStaleValidators(t *testing.T) {
	h := newHarness(t, herrforsMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("ETag", `"abc123"`)
		w.Header().Set("Last-Modified", "Wed, 27 Aug 2026 10:00:00 GMT")
		w.Header().Set("Accept-Ranges", "bytes")
		io.WriteString(w, `<a href="`+canonical+`/x">t</a>`)
	})
	res, _ := h.get(t, variantHost, "/")
	for _, k := range []string{"Content-Length", "ETag", "Last-Modified", "Accept-Ranges"} {
		if v := res.Header.Get(k); v != "" {
			t.Errorf("%s survived on a rewritten response: %q", k, v)
		}
	}
}

// TestHeaderOriginsRewritten covers tests 1, 11 and 23.
func TestHeaderOriginsRewritten(t *testing.T) {
	h := newHarness(t, herrforsMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Security-Policy", "default-src 'self' "+canonical)
		w.Header().Set("Link", "<"+canonical+`/wp-json/>; rel="https://api.w.org/"`)
		io.WriteString(w, "x")
	})
	res, _ := h.get(t, variantHost, "/")
	for _, k := range []string{"Content-Security-Policy", "Link"} {
		v := res.Header.Get(k)
		if strings.Contains(v, canonical) {
			t.Errorf("%s still carries the canonical origin: %q", k, v)
		}
		if !strings.Contains(v, variant) {
			t.Errorf("%s was not rewritten: %q", k, v)
		}
	}
	if !strings.Contains(res.Header.Get("Link"), "https://api.w.org/") {
		t.Errorf("Link lost its third-party rel URI: %q", res.Header.Get("Link"))
	}
}

// TestRequestDirectionMechanics covers the PLAN §5.7 items that are silent
// failures.
func TestRequestDirectionMechanics(t *testing.T) {
	h := newHarness(t, herrforsMap(t), html("<p>x</p>"))
	req, _ := http.NewRequest("GET", h.front.URL+"/", nil)
	req.Host = variantHost
	req.Header.Set("X-Forwarded-Port", "8081")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	if h.seen.Host != "www.herrfors.fi" {
		t.Errorf("Host is %q — SetURL clears Out.Host and it must be reassigned after", h.seen.Host)
	}
	if got := h.seen.Header.Get("X-Forwarded-Proto"); got != "https" {
		t.Errorf("X-Forwarded-Proto is %q — SetXForwarded writes http and must be overridden after it", got)
	}
	if got := h.seen.Header.Get("X-Forwarded-Port"); got != "" {
		t.Errorf("X-Forwarded-Port was forwarded as %q; it is not hop-by-hop and must be deleted (PLAN §2.3)", got)
	}
	if got := h.seen.Header.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("Accept-Encoding upstream is %q, want identity", got)
	}
}

// TestNonRewritableNeverEntersARewriter is acceptance test 25, and test 12 for
// the binary case.
func TestNonRewritableNeverEntersARewriter(t *testing.T) {
	binary := append([]byte("\x89PNG\r\n\x1a\n"), []byte(canonical+"/x")...)
	for _, ct := range []string{"image/png", "text/css", "application/javascript", "application/json"} {
		h := newHarness(t, herrforsMap(t), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			w.Write(binary)
		})
		_, got := h.get(t, variantHost, "/")
		if !bytes.Equal(got, binary) {
			t.Errorf("%s: body was not byte-identical", ct)
		}
		if n := h.stats.Rewrites(rewrite.SurfaceHTMLAttr); n != 0 {
			t.Errorf("%s: html-attr counter is %d, want 0 — it entered a rewriter", ct, n)
		}
	}
}

// TestUpstreamFailureSurfaced is acceptance test 14.
func TestUpstreamFailureSurfaced(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:1") // nothing listens there
	p := &Proxy{Upstream: target, Map: herrforsMap(t), Stats: rewrite.NewStats(false)}
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	req, _ := http.NewRequest("GET", front.URL+"/", nil)
	req.Host = variantHost
	res, err := http.DefaultClient.Do(req)
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

// TestFilterMatchesProxyBytes is acceptance test 27, over every real page in the
// corpus: the Unix filter and the proxy must be the same engine.
func TestFilterMatchesProxyBytes(t *testing.T) {
	m := herrforsMap(t)
	for _, f := range corpus(t) {
		in, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		filtered, err := io.ReadAll(rewrite.NewHTML(bytes.NewReader(in), m.Forward(), nil, rewrite.Options{}))
		if err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, m, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=UTF-8")
			w.Write(in)
		})
		_, proxied := h.get(t, variantHost, "/")
		if !bytes.Equal(filtered, proxied) {
			t.Errorf("%s: filter and proxy disagree (%d vs %d bytes)", f, len(filtered), len(proxied))
		}
	}
}

// TestIdentityMapThroughProxy is test 24 on the proxy path.
func TestIdentityMapThroughProxy(t *testing.T) {
	m, err := origin.NewMap([]origin.Site{{
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
		h := newHarness(t, m, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Write(in)
		})
		_, got := h.get(t, "www.herrfors.fi", "/")
		if !bytes.Equal(in, got) {
			t.Errorf("%s: identity map through the proxy changed the bytes (%d -> %d)", f, len(in), len(got))
		}
	}
}

// TestDryRunServesUnmodified: --dry-run is safe to point at a live canonical
// checkout (PLAN §5.8).
func TestDryRunServesUnmodified(t *testing.T) {
	body := `<a href="` + canonical + `/x">t</a>`
	h := newHarness(t, herrforsMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("ETag", `"abc123"`)
		w.Header().Set("Location", canonical+"/y")
		io.WriteString(w, body)
	}, func(p *Proxy) { p.DryRun = true })

	res, got := h.get(t, variantHost, "/")
	if string(got) != body {
		t.Errorf("--dry-run changed the body:\n got %q\nwant %q", got, body)
	}
	if v := res.Header.Get("Location"); v != canonical+"/y" {
		t.Errorf("--dry-run changed Location to %q", v)
	}
	if res.Header.Get("ETag") == "" {
		t.Error("--dry-run dropped the ETag; it must change nothing")
	}
	if h.stats.Total() == 0 {
		t.Error("--dry-run counted no rewrites; it must still report what it would have done")
	}
}
