package corpus

import (
	"bytes"
	"context"
	"fmt"
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

// The scorer must run the arm the proxy would run for this content type.
//
// originsIn ran NewResponseBody — the HTML pipeline — on every body, while the
// proxy dispatches every `*xml` type to HostLeaksXMLCounted. The HTML pipeline
// applies the reference view only where an HTML parser decodes one: attributes
// and foreign content. Element content in an ordinary XML element is the gap,
// and that is where every sitemap `<loc>` and every RSS `<link>` lives — so an
// unrewritten feed scored GREEN, on the content types the proxy grew a
// dedicated arm for, and against exactly the leak class two rounds were spent
// fixing.
//
// The `<link href>` case is the control that shows it was a dispatch bug and
// not a decode bug: an attribute, so the HTML parser's own entity pass caught it
// and that one shape always worked.
func TestTheScorerRunsTheArmTheProxyWouldRun(t *testing.T) {
	m, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://www.canon.test"),
		Variant:   origin.MustParse("https://v.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, ctype, body string }{
		{"a sitemap loc, reference-encoded", "application/xml",
			`<urlset><url><loc>https:&#47;&#47;www.canon.test/x</loc></url></urlset>`},
		{"an rss link, reference-encoded", "application/rss+xml",
			`<rss><channel><item><link>https:&#47;&#47;www.canon.test/x</link></item></channel></rss>`},
		{"an rss guid", "application/rss+xml",
			`<rss><channel><item><guid>https:&#47;&#47;www.canon.test/x</guid></item></channel></rss>`},
		{"a sitemap loc, css-escaped", "application/xml",
			`<urlset><url><loc>https\3a \2f \2f www.canon.test/x</loc></url></urlset>`},
		{"css escapes in plain text", "text/plain",
			`a{background:url(https\3a \2f \2f www.canon.test/x)}`},
		{"an atom link href, which always worked", "application/atom+xml",
			`<feed><entry><link href="https:&#47;&#47;www.canon.test/x"/></entry></feed>`},
		{"an ordinary page, which always worked", "text/html",
			`<a href="https://www.canon.test/x">t</a>`},
	} {
		t.Run(c.name, func(t *testing.T) {
			leaks, tier2 := countLeaks(m, response{body: []byte(c.body), contentType: c.ctype})
			if leaks == 0 && tier2 == 0 {
				t.Errorf("a canonical origin the proxy rewrites was scored clean "+
					"under %s:\n%s", c.ctype, c.body)
			}
		})
	}
}

// The assertion that would have caught rounds twenty-two through twenty-six on
// their first run.
//
// Every other check here compares the proxy's output against the scorer's, so
// when both are wrong in the same way the run is GREEN. Five consecutive rounds
// of silent wp_options destruction went unreported by the run PLAN §7 calls
// "the only test that validates against reality". A serialized value either
// parses or it does not; that is a fact about the served bytes alone.
func TestServedSerializedPayloadsMustParse(t *testing.T) {
	str := func(v string) string { return `s:` + strconv.Itoa(len(v)) + `:"` + v + `";` }
	good := `a:1:{s:3:"css";` + str(`.a{color:red}`) + `}`
	// The measured shape: a length six bytes short of its data.
	bad := `a:1:{s:3:"css";s:` + strconv.Itoa(len(".a{color:red}")-6) +
		`:"` + `.a{color:red}` + `";}`

	// The count is a detector, not a census — a container fails when its child
	// does, so one broken value can raise it more than once. What has to be
	// exact is zero versus not-zero.
	for _, c := range []struct {
		name, body string
		broken     bool
	}{
		{"a valid payload", good, false},
		{"a stale length", bad, true},
		{"prose that merely resembles one", `The value is s:6:"a.test"; ok`, false},
		{"no serialized data at all", `<a href="https://x/">t</a>`, false},
		{"a bare URL, which is most bodies", `see https://x/a and https://x/b`, false},
		{"percent-encoded and stale", `o=s%3A5%3A%22abcdefgh%22%3B`, true},
		{"every other serialized type", `a:2:{i:0;N;i:1;R:2;}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := rewrite.BrokenSerialized([]byte(c.body))
			if (got > 0) != c.broken {
				t.Errorf("counted %d, want broken=%v:\n%s", got, c.broken, c.body)
			}
		})
	}
}

// And the same assertion where it actually runs: a full compare over a variant
// that serves a stale length must not be GREEN.
//
// This is the shape that five rounds could not see. Both sides of the byte
// comparison agreed, no canonical origin was present, and the run reported
// "no canonical origin reached the browser, no page re-serialised".
func TestAStaleLengthIsNotAGreenRun(t *testing.T) {
	css := ".a{color:red}"
	valid := `a:1:{s:3:"css";s:` + strconv.Itoa(len(css)) + `:"` + css + `";}`
	stale := `a:1:{s:3:"css";s:` + strconv.Itoa(len(css)-6) + `:"` + css + `";}`

	// Valid at the canonical, broken at the variant: the proxy did this. The
	// same body on both sides is a database that was already broken, which the
	// baseline subtraction correctly attributes elsewhere — see
	// TestAPreBrokenRowIsNotBlamedOnTheProxy.
	r := compareBodies(t, valid, stale)
	if r.BrokenSerialized == 0 {
		t.Fatalf("a served stale length was not detected:\n%s", stale)
	}
	var buf bytes.Buffer
	if WriteReport(&buf, []Result{r}) {
		t.Errorf("the run was GREEN with a broken payload on the page:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "does not describe the data") {
		t.Errorf("the report does not say what is wrong:\n%s", buf.String())
	}
}

// compareBodies runs one comparison against two httptest servers.
func compareBodies(t *testing.T, canonBody, variantBody string) Result {
	t.Helper()
	m, err := origin.NewMap([]origin.Site{{
		Name: "main", Canonical: origin.MustParse("https://www.canon.test"),
		Variant: origin.MustParse("https://v.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	serve := func(body string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(s.Close)
		return s
	}
	cs, vs := serve(canonBody), serve(variantBody)
	cu, _ := url.Parse(cs.URL)
	vu, _ := url.Parse(vs.URL)
	return compare(context.Background(), Options{
		Canonical: cu, Variant: vu, Map: m, Client: cs.Client(),
	}, "/")
}

// A row that was already broken in the database is not the proxy's doing.
//
// Real WordPress databases carry these — from the careless search-replace
// hostshift exists to avoid — and counting the variant body alone made every
// such site RED forever, on bytes the proxy passed through untouched. A check
// that is always RED is a check nobody reads, which is the mechanism that let
// five rounds of real corruption go unnoticed.
func TestAPreBrokenRowIsNotBlamedOnTheProxy(t *testing.T) {
	css := ".a{color:red}"
	stale := `a:1:{s:3:"css";s:` + strconv.Itoa(len(css)-6) + `:"` + css + `";}`

	r := compareBodies(t, stale, stale)
	if r.BrokenSerialized != 0 {
		t.Errorf("a row broken on both sides was blamed on the proxy: %d", r.BrokenSerialized)
	}
	var buf bytes.Buffer
	if !WriteReport(&buf, []Result{r}) {
		t.Errorf("the run went RED for a payload the proxy did not touch:\n%s", buf.String())
	}
}

// Correctly escaped content is not broken content.
//
// A serialized option reaches the browser through `esc_attr`, `esc_textarea` or
// a JSON string, and in every one of those the quotes are escaped. A detector
// that knows only the literal and percent spellings called all of them broken —
// so a healthy WordPress settings page turned the run RED permanently, on bytes
// the proxy had handled perfectly. A check that is always RED is a check nobody
// reads.
func TestEscapedSerializedContentIsNotBroken(t *testing.T) {
	css := `body{color:red}`
	blob := `a:1:{s:3:"css";s:` + strconv.Itoa(len(css)) + `:"` + css + `";}`
	for _, c := range []struct{ name, body string }{
		{"raw", blob},
		{"esc_attr", `<input value="` + strings.ReplaceAll(blob, `"`, "&quot;") + `">`},
		{"esc_textarea", `<textarea>` + strings.ReplaceAll(blob, `"`, "&quot;") + `</textarea>`},
		{"a JSON string", `{"o":"` + strings.ReplaceAll(blob, `"`, `\"`) + `"}`},
		{"a URL path that contains s:", `https://x/s:3:"a"`},
		{"minified CSS", `nav a:hover{color:red}d:\shares\logo.png`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if n := rewrite.BrokenSerialized([]byte(c.body)); n != 0 {
				t.Errorf("counted %d broken values in correctly formed content:\n%s",
					n, c.body)
			}
		})
	}
}

// A byte-identical page is counted as byte-identical even when it carries a
// note.
//
// `equal++` sat in the last arm of a switch whose earlier arms match on errors,
// leaks, broken payloads and Tier 2 — so a page whose BYTES column said `same`
// was reported in the summary as `0 byte-identical`. That summary line is what
// a developer reads.
func TestAByteIdenticalPageIsCountedEvenWithANote(t *testing.T) {
	// A page that is byte-identical *and* carries a note: a Tier 2 type holding
	// an origin, which is reported loudly and deliberately does not fail the
	// run. Built directly, because the point is what WriteReport does with it.
	var buf bytes.Buffer
	WriteReport(&buf, []Result{{
		Path: "/style.css", Equal: true, Tier2: 3, ContentType: "text/css",
	}})
	out := buf.String()
	if !strings.Contains(out, "1 byte-identical") {
		t.Errorf("a page the table calls `same` was not counted:\n%s", out)
	}
	if !strings.Contains(out, "Tier 2") {
		t.Errorf("the note was lost:\n%s", out)
	}
}
