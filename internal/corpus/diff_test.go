package corpus

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

const (
	canonicalOrigin = "https://c.example"
	variantOrigin   = "https://v.example"
)

func testMap(t *testing.T) *origin.Map {
	t.Helper()
	m, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse(canonicalOrigin),
		Variant:   origin.MustParse(variantOrigin),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// site serves pages keyed by path, with links between them so the crawler has
// something to follow.
func site(t *testing.T, pages map[string]string) *url.URL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// proxied serves each canonical page through the real rewriter, standing in for
// hostshift.
func proxied(t *testing.T, pages map[string]string) *url.URL {
	t.Helper()
	m := testMap(t)
	out := map[string]string{}
	for p, body := range pages {
		b, err := io.ReadAll(rewrite.NewResponseBody(
			strings.NewReader(body), m.Forward(), nil, rewrite.Options{}))
		if err != nil {
			t.Fatal(err)
		}
		out[p] = string(b)
	}
	return site(t, out)
}

// TestGreenRun is the shape of a passing corpus diff: the proxy's bytes equal
// the canonical bytes put through the same engine.
func TestGreenRun(t *testing.T) {
	pages := map[string]string{
		"/":       `<a href="` + canonicalOrigin + `/a">a</a><a href="` + canonicalOrigin + `/b">b</a>`,
		"/a":      `<img src="` + canonicalOrigin + `/x.png">`,
		"/b":      `<p>nothing here</p>`,
		"/nolink": `<p>unreachable by crawl</p>`,
	}
	results, err := Run(context.Background(), Options{
		Canonical: site(t, pages), Variant: proxied(t, pages), Map: testMap(t), N: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("crawled %d pages, want 3 (/, /a, /b — /nolink is unreachable)", len(results))
	}
	var buf bytes.Buffer
	if !WriteReport(&buf, results) {
		t.Errorf("run should be green:\n%s", buf.String())
	}
	for _, r := range results {
		if !r.Equal {
			t.Errorf("%s: not byte-identical", r.Path)
		}
		if r.Leaks != 0 {
			t.Errorf("%s: %d leaks", r.Path, r.Leaks)
		}
	}
}

// TestLeakFailsTheRun is the assertion the whole exercise exists for: a
// canonical origin reaching the browser is never innocent, whatever else matches.
func TestLeakFailsTheRun(t *testing.T) {
	canonical := map[string]string{"/": `<a href="` + canonicalOrigin + `/a">a</a>`}
	// A "proxy" that forgot to rewrite.
	leaky := map[string]string{"/": `<a href="` + canonicalOrigin + `/a">a</a>`}

	results, err := Run(context.Background(), Options{
		Canonical: site(t, canonical), Variant: site(t, leaky), Map: testMap(t), N: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Leaks == 0 {
		t.Fatal("a canonical origin in the variant response was not counted as a leak")
	}
	var buf bytes.Buffer
	if WriteReport(&buf, results) {
		t.Errorf("a run with leaks must not be green:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "CANONICAL ORIGIN REACHED THE BROWSER") {
		t.Errorf("the report does not name the failure:\n%s", buf.String())
	}
}

// TestLineCountChangeFailsTheRun: splicing never rebuilds whitespace, so a
// line-count change means something re-serialised — the lol-html failure mode
// §5.7 rejected Rust for.
func TestLineCountChangeFailsTheRun(t *testing.T) {
	canonical := map[string]string{"/": "<a href=\"" + canonicalOrigin + "/a\"\n  class=\"k\">a</a>\n"}
	// Same links, but re-serialised onto one line.
	reserialised := map[string]string{"/": `<a href="` + variantOrigin + `/a" class="k">a</a>` + "\n"}

	results, err := Run(context.Background(), Options{
		Canonical: site(t, canonical), Variant: site(t, reserialised), Map: testMap(t), N: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Leaks != 0 {
		t.Fatalf("this fixture should leak nothing; it tests line counts")
	}
	var buf bytes.Buffer
	if WriteReport(&buf, results) {
		t.Errorf("a run whose line count changed must not be green:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "re-serialised") {
		t.Errorf("the report does not name the failure:\n%s", buf.String())
	}
}

// TestDynamicContentIsNotAFailure: a live site differs between two fetches for a
// dozen innocent reasons. Byte inequality alone must not fail the run, or the
// diff is unusable against anything real.
func TestDynamicContentIsNotAFailure(t *testing.T) {
	var n int
	nonce := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<p nonce=%q>x</p>\n", fmt.Sprint(n))
	}))
	defer nonce.Close()
	u, _ := url.Parse(nonce.URL)

	results, err := Run(context.Background(), Options{
		Canonical: u, Variant: u, Map: testMap(t), N: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Equal {
		t.Fatal("the fixture must differ between fetches or it tests nothing")
	}
	var buf bytes.Buffer
	if !WriteReport(&buf, results) {
		t.Errorf("differing-but-clean pages must not fail the run:\n%s", buf.String())
	}
}

// TestCrawlStaysOnTheSite: a crawl that wanders onto a third-party host is
// measuring nothing.
func TestCrawlStaysOnTheSite(t *testing.T) {
	pages := map[string]string{
		"/":     `<a href="https://third-party.example/x">off-site</a><a href="/a">a</a>`,
		"/a":    `<p>a</p>`,
		"/deep": `<p>unreferenced</p>`,
	}
	results, err := Run(context.Background(), Options{
		Canonical: site(t, pages), Variant: proxied(t, pages), Map: testMap(t), N: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if strings.Contains(r.Path, "third-party") {
			t.Errorf("the crawl left the site: %s", r.Path)
		}
	}
	if len(results) != 2 {
		t.Errorf("crawled %d pages, want 2", len(results))
	}
}

// TestCrawlFollowsCanonicalHostLinks: a page's links carry the hosts the
// *database* holds, not the host being fetched from. They differ whenever
// --canonical-base points somewhere else, which is how the diff is run against a
// local site instead of against production.
func TestCrawlFollowsCanonicalHostLinks(t *testing.T) {
	pages := map[string]string{
		"/":  `<a href="` + canonicalOrigin + `/a">a</a>`,
		"/a": `<p>a</p>`,
	}
	results, err := Run(context.Background(), Options{
		Canonical: site(t, pages), Variant: proxied(t, pages), Map: testMap(t), N: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("crawled %d pages, want 2 — a link on the canonical *origin* was not followed", len(results))
	}
}

// TestExplicitPathsSkipTheCrawl.
func TestExplicitPathsSkipTheCrawl(t *testing.T) {
	pages := map[string]string{"/": `<p>x</p>`, "/only": `<p>y</p>`}
	results, err := Run(context.Background(), Options{
		Canonical: site(t, pages), Variant: proxied(t, pages), Map: testMap(t),
		Paths: []string{"/only"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != "/only" {
		t.Fatalf("got %+v, want just /only", results)
	}
}

// TestResolveRedirectsTheFetch is the flag that keeps the diff away from a
// client's live site: under production-canonical the canonical base *is* the
// production hostname.
func TestResolveRedirectsTheFetch(t *testing.T) {
	pages := map[string]string{"/": `<p>local</p>`}
	local := site(t, pages)

	// A base that would resolve to something else entirely, pinned to the local
	// server. If --resolve did not work this would fail to connect rather than
	// quietly measure the wrong thing.
	base, _ := url.Parse("http://www.definitely-not-real.test:80")
	results, err := Run(context.Background(), Options{
		Canonical: base, Variant: local, Map: testMap(t), Paths: []string{"/"},
		Resolve: map[string]string{"www.definitely-not-real.test:80": local.Host},
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Err != nil {
		t.Fatalf("--resolve did not redirect the fetch: %v", results[0].Err)
	}
	if !results[0].Equal {
		t.Errorf("the resolved fetch did not return the local page")
	}
}

// TestCanonicalHeadersAppliedToCanonicalOnly: the header supplies what the
// TLS-terminating router would have added when --resolve points past it, and
// must not be sent to the variant, which gets it from hostshift.
func TestCanonicalHeadersAppliedToCanonicalOnly(t *testing.T) {
	var canonicalSaw, variantSaw string
	mk := func(dst *string) *url.URL {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*dst = r.Header.Get("X-Forwarded-Proto")
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<p>x</p>")
		}))
		t.Cleanup(srv.Close)
		u, _ := url.Parse(srv.URL)
		return u
	}
	_, err := Run(context.Background(), Options{
		Canonical: mk(&canonicalSaw), Variant: mk(&variantSaw), Map: testMap(t),
		Paths:            []string{"/"},
		CanonicalHeaders: map[string]string{"X-Forwarded-Proto": "https"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if canonicalSaw != "https" {
		t.Errorf("the canonical fetch did not carry the header: %q", canonicalSaw)
	}
	if variantSaw != "" {
		t.Errorf("the variant fetch carried the canonical-only header: %q", variantSaw)
	}
}

// countLeaks ran every body through the HTML pipeline regardless of what the
// proxy would have done with it, so three different situations all scored the
// same RED.
//
// An attachment is skipped by design whatever it contains, and the `<a href>`
// crawler reaches download URLs routinely on a WooCommerce store — that was a
// RED verdict for correct behaviour. A Tier 2 type is different: PLAN's fast
// path excludes `text/css` and the JavaScript types "and added only if the
// corpus diff shows a leak", so an origin there is this tool's designed trigger
// and has to be reported as one rather than as a proxy defect. Everything else
// is a real leak.
func TestVerdictFollowsWhatTheProxyActuallyDoes(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.canon.test"),
		Variant:   origin.MustParse("https://v.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	const body = `<a href="https://www.canon.test/x">t</a>`
	const css = `@font-face{src:url(https://www.canon.test/f.woff2)}`

	for _, c := range []struct {
		name         string
		r            response
		leaks, tier2 int
	}{
		{"html carrying an origin is a leak",
			response{body: []byte(body), contentType: "text/html; charset=utf-8"}, 1, 0},
		{"a stylesheet is the PLAN's trigger, not a defect",
			response{body: []byte(css), contentType: "text/css"}, 0, 1},
		{"so is JavaScript, under either spelling",
			response{body: []byte(body), contentType: "text/javascript"}, 0, 1},
		{"an attachment is skipped by design, whatever it holds",
			response{body: []byte(body), contentType: "text/html", attachment: true}, 0, 0},
		{"a clean page is clean",
			response{body: []byte(`<a href="/x">t</a>`), contentType: "text/html"}, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			leaks, tier2 := countLeaks(m, c.r)
			if leaks != c.leaks || tier2 != c.tier2 {
				t.Errorf("got leaks=%d tier2=%d, want leaks=%d tier2=%d",
					leaks, tier2, c.leaks, c.tier2)
			}
		})
	}
}
