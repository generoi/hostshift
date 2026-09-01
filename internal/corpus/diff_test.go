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
	// Four: the crawl follows subresources as well as links, because the Tier 2
	// count it exists to produce comes from a *response* body — so `/x.png` is
	// fetched too, and only `/nolink`, which nothing points at, is missed.
	if len(results) != 4 {
		t.Fatalf("crawled %d pages, want 4 (/, /a, /b, /x.png — /nolink is unreachable)", len(results))
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

// TestLineCountChangeIsReported: splicing never rebuilds whitespace, so a
// line-count change is worth naming — the lol-html failure mode §5.7 rejected
// Rust for.
//
// Reported, not fatal, and the demotion is deliberate. Under
// production-canonical the canonical hostname is not routed locally, so the
// canonical fetch carries a different Host than the proxy sends upstream, and
// WordPress emits one extra `<link rel="dns-prefetch">` for every asset host
// that is not SERVER_NAME. Exactly one line, on every page, on a site with
// nothing wrong with it — so this fired on every row of a stock Bedrock crawl,
// in the mode the README calls the one where the hazards live. A verdict that
// is red on every healthy page stops being read, and on the run that carried 32
// genuinely broken values the real signal was one clause appended to a phrase
// that fires regardless.
//
// Nothing is lost that is not covered better elsewhere. A rewriter that
// re-serialised HTML would change bytes under an identity map, which is test 24
// and is asserted directly in the Go suite; a re-serialised *value* is what
// `broken` and `unread` ask PHP's own question about. This was an inference
// standing in for tests that did not exist yet.
func TestLineCountChangeIsReported(t *testing.T) {
	// A page of ordinary length missing exactly one line, which is the shape
	// the mechanism actually produces: one `<link rel="dns-prefetch">` for an
	// asset host that is SERVER_NAME on one fetch and not on the other.
	//
	// The fixture used to be two lines collapsed to one, and that is not this
	// shape — half a document is missing, which is what a truncated response
	// looks like, and the bounded assertion cannot tell the two apart on a
	// page that short. It should not have to: nothing on a real site is two
	// lines long.
	var body strings.Builder
	body.WriteString("<html>\n<head>\n")
	body.WriteString(`<link rel="dns-prefetch" href="//cdn.example">` + "\n")
	body.WriteString("</head>\n<body>\n")
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&body, "<p>line %d</p>\n", i)
	}
	withPrefetch := body.String() + `<a href="` + canonicalOrigin + `/a">a</a>` + "\n</body>\n</html>\n"
	// The same page, rewritten, minus that one Host-dependent line.
	withoutPrefetch := strings.Replace(
		strings.Replace(withPrefetch, canonicalOrigin, variantOrigin, 1),
		`<link rel="dns-prefetch" href="//cdn.example">`+"\n", "", 1)

	canonical := map[string]string{"/": withPrefetch}
	reserialised := map[string]string{"/": withoutPrefetch}

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
	green := WriteReport(&buf, results)
	// Green, because that is the whole demotion: a line-count change alone must
	// not fail a run. Assert the verdict and not only the wording — the note can
	// be printed by a version that still sends the run RED, and that version is
	// the bug this test exists to keep out.
	if !green {
		t.Errorf("a line-count change alone must not fail the run:\n%s", buf.String())
	}
	// Named, with both counts, so a developer can see what moved.
	if !strings.Contains(buf.String(), "line count 14→13") {
		t.Errorf("the report does not name the change:\n%s", buf.String())
	}
	// And it points at the tests that answer the question it used to guess at.
	if !strings.Contains(buf.String(), "re-serialisation") {
		t.Errorf("the report does not say where to look:\n%s", buf.String())
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
	if !strings.Contains(buf.String(), "PHP will refuse or truncate") {
		t.Errorf("the report does not say what is wrong:\n%s", buf.String())
	}
	// And it does not claim the count is a number of values. One stale length
	// fails every container around it, so the number is a detector; saying
	// "N serialized value(s) … with a length that does not describe the data"
	// read it as a census and was wrong by the nesting depth.
	if strings.Contains(buf.String(), "serialized value(s) served") {
		t.Errorf("the report states the count as a census of values:\n%s", buf.String())
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

// A page that both leaks an origin and serves a blob PHP refuses reports both.
//
// The note was a switch, so the first arm to match won and the rest were
// silent. The page most likely to earn two notes is the one carrying a
// serialized payload full of URLs — exactly the shape this detector exists for
// — so the arm that lost was the one worth reading.
func TestAPageReportsEveryNoteItEarned(t *testing.T) {
	var buf bytes.Buffer
	WriteReport(&buf, []Result{{
		Path: "/both", Leaks: 2, BrokenSerialized: 3, ContentType: "text/html",
	}})
	out := buf.String()
	if !strings.Contains(out, "CANONICAL ORIGIN REACHED THE BROWSER") {
		t.Errorf("the leak was not reported:\n%s", out)
	}
	if !strings.Contains(out, "3 header(s) failed to parse") {
		t.Errorf("the broken payload was not reported:\n%s", out)
	}
	// And the summary names both, so a RED run says what was wrong without
	// scrolling back up.
	if !strings.Contains(out, "2 leaks, 3 broken") {
		t.Errorf("the summary did not count both:\n%s", out)
	}
}

// The report names a page whose serialized value was rewritten in a spelling
// this build cannot read.
//
// It is a separate column from `broken` because it answers a different
// question, and the difference is what makes it survive the canonical baseline:
// `broken` asks whether the served bytes parse, which is false on both sides
// for a spelling nothing reads, so the subtraction cancels it. This asks
// whether the rewrite edited bytes it could not account for, which the
// canonical pass never does.
func TestTheReportNamesAnUnreadableRewrite(t *testing.T) {
	var buf bytes.Buffer
	WriteReport(&buf, []Result{{
		Path: "/x", Equal: true, UnreadRewrites: 1, ContentType: "text/html",
	}})
	out := buf.String()
	if !strings.Contains(out, "cannot read") {
		t.Errorf("the page was not named:\n%s", out)
	}
	// One per page, not a count of spans. The signal is "look at this page";
	// claiming an arithmetic the evidence does not support is what the first
	// version of this did, and it was wrong in both directions.
	if !strings.Contains(out, "1 unread") {
		t.Errorf("the summary did not count it:\n%s", out)
	}
	if strings.Contains(out, "GREEN") {
		t.Errorf("a page rewritten in a spelling nothing reads was called green:\n%s", out)
	}
}

// The scorer reports a page whose serialized value the rewrite touched in a
// spelling the walk cannot read — and reports it on the variant side only, so
// the canonical baseline does not cancel it.
//
// This is the end-to-end half of the guarantee. Asserting the counter in the
// rewrite package proves the arithmetic; only running an actual comparison
// proves the scorer asks for it at all.
func TestTheScorerSeesARewriteItCouldNotRead(t *testing.T) {
	canon := "https://www.canon.test"
	variant := "https://v.ddev.site"
	url := canon + "/a.png"
	blob := `a:1:{i:0;s:` + strconv.Itoa(len(url)) + `:"` + url + `";}`
	// JSON_HEX_QUOT composed with percent-encoding: two encoders the walk reads
	// separately and not together, so nothing re-emits the length.
	wire := strings.ReplaceAll(blob, `"`, `%5Cu0022`)
	served := strings.ReplaceAll(wire, "www.canon.test", "v.ddev.site")

	r := compareBodies(t, wire, served)
	if r.UnreadRewrites == 0 {
		t.Errorf("the scorer did not see it:\n canon   %s\n variant %s", wire, served)
	}
	// And a page in a spelling the walk *does* read is not reported.
	clean := compareBodies(t, blob, strings.ReplaceAll(blob, "www.canon.test", "v.ddev.site"))
	if clean.UnreadRewrites != 0 {
		t.Errorf("a readable spelling was reported as unread: %d", clean.UnreadRewrites)
	}
	_ = variant
}

// A Tier 2 body carrying an unreadable serialized value is still not scored.
//
// The proxy does not rewrite text/css or JavaScript at all, so "we changed
// bytes we could not read" cannot be true of one — and WriteReport's Tier 2 arm
// deliberately refuses to turn the run red for these, because the proxy is
// doing what it says it does. Without the guard the new column contradicts the
// one two arms below it.
//
// The fixture needs a real serialized header, not just CSS that looks like one:
// `border:1px` fails readLen, so a body without a genuine `s:NN:` would pass
// this test whether the guard existed or not.
func TestATier2BodyWithARealHeaderIsStillNotScored(t *testing.T) {
	canon := "https://www.canon.test"
	target := canon + "/a.png"
	blob := `a:1:{i:0;s:` + strconv.Itoa(len(target)) + `:"` + target + `";}`
	// An encoding the walk cannot read, inside a stylesheet comment.
	wire := `.a{color:red}/* ` + strings.ReplaceAll(blob, `"`, `%5Cu0022`) + ` */`

	m, err := origin.NewMap([]origin.Site{{
		Name: "main", Canonical: origin.MustParse(canon),
		Variant: origin.MustParse("https://v.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	serve := func(body, ct string) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(s.Close)
		return s
	}
	cs := serve(wire, "text/css")
	vs := serve(wire, "text/css")
	cu, _ := url.Parse(cs.URL)
	vu, _ := url.Parse(vs.URL)
	r := compare(context.Background(), Options{
		Canonical: cu, Variant: vu, Map: m, Client: cs.Client(),
	}, "/style.css")
	if r.UnreadRewrites != 0 {
		t.Errorf("a Tier 2 body was scored, which contradicts the Tier 2 arm below it")
	}
}
