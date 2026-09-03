package corpus

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 45, on 8fabead ("Fit each condition to its defect").
//
// 8fabead taught `links` to follow `<link href>`, `<script src>`, `<img src>`
// and `<iframe src>`, so a default run can reach a stylesheet and let the Tier 2
// line fire. The change is right about the gap and wrong about the budget.

// r45Page is the shape of a stock WordPress homepage: the subresources are in
// `<head>` and the links to other pages are in `<body>`, so the subresources are
// enqueued first and the queue is breadth-first.
func r45Page(assets, pages int) string {
	var b strings.Builder
	b.WriteString("<html><head>")
	for i := 0; i < assets; i++ {
		fmt.Fprintf(&b, `<link rel="stylesheet" href="/wp-content/themes/t/a%d.css?ver=1">`, i)
		fmt.Fprintf(&b, `<script src="/wp-includes/js/s%d.js?ver=1"></script>`, i)
	}
	b.WriteString("</head><body>")
	for i := 0; i < pages; i++ {
		fmt.Fprintf(&b, `<a href="/page-%d/">p%d</a>`, i, i)
	}
	b.WriteString("</body></html>")
	return b.String()
}

// TestR45TheCrawlBudgetIsSpentOnAssets.
//
// `crawl` stops at `o.N` *paths*, and `hostshift diff`'s default is `-n 20`.
// Every subresource now competes for that budget with the pages, and it wins,
// because a WordPress `<head>` is emitted before the `<body>` and the queue is
// FIFO. Measured below on a page with an ordinary number of enqueued assets:
// the default run compares the homepage and nineteen files from its head, and
// **not one other page**.
//
// That is not a wash. Tier 2 is documented as *not* failing a run — WriteReport
// prints it and leaves `green` alone — so what the change buys is a louder note,
// and what it spends is nineteen pages of the only check in the tool that can
// turn RED for invariant 28. The README calls this "the only test that validates
// against reality"; before 8fabead it validated twenty pages and now it
// validates one.
//
// The fix is a budget per kind rather than one shared queue: crawl to `-n`
// *pages* as before, and fetch the subresources those pages reference on top of
// it (or behind their own flag). Nothing about reaching a stylesheet requires
// spending a page slot on it.
func TestR45TheCrawlBudgetIsSpentOnAssets(t *testing.T) {
	home := r45Page(15, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte(home))
			return
		}
		_, _ = w.Write([]byte("<html><body>x</body></html>"))
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	m, err := origin.NewMap([]origin.Site{{
		Name:      "r45",
		Canonical: origin.MustParse(srv.URL),
		Variant:   origin.MustParse(srv.URL),
	}})
	if err != nil {
		t.Fatal(err)
	}

	// -n 20, the documented default of `hostshift diff`.
	paths, err := crawl(context.Background(), Options{
		Canonical: base, Variant: base, Map: m, N: 20, Client: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}

	var pages, assets int
	for _, p := range paths {
		switch {
		case strings.HasPrefix(p, "/page-"), p == "/":
			pages++
		default:
			assets++
		}
	}
	t.Logf("crawled %d paths: %d pages, %d assets\n%s", len(paths), pages, assets,
		strings.Join(paths, "\n"))
	if pages < 2 {
		t.Errorf("the whole -n 20 budget went to subresources: %d page(s), %d asset(s).\n"+
			"Before 8fabead this crawl compared 11 pages; it now compares 1 and 19 files "+
			"from its head, and Tier 2 — the reason the subresources were added — does not "+
			"fail a run.", pages, assets)
	}
}

// TestR45AnAssetOnlyCrawlDoesNotClaimTheInvariant28Verdict.
//
// Every row is a `text/css` file, which the proxy is documented not to rewrite.
// Each is byte-identical, so nothing clears `green` — and the run used to print
// "corpus diff GREEN: no canonical origin reached the browser", a sentence about
// HTML over a table with no HTML in it. `len(results) > 0` is satisfied by
// twenty rows that answered a different question.
//
// Still green: Tier 2 must not fail a run, which is
// TestATier2BodyTheProxyNeverRewritesIsNotAnUnreadRewrite. What it must not do
// is claim an invariant nothing in the run tested.
func TestR45AnAssetOnlyCrawlDoesNotClaimTheInvariant28Verdict(t *testing.T) {
	var results []Result
	for i := 0; i < 20; i++ {
		results = append(results, Result{
			Path:        fmt.Sprintf("/wp-content/themes/t/a%d.css", i),
			Equal:       true,
			ContentType: "text/css",
			// Byte-identical, so lines and bytes agree and nothing is cleared.
			LinesCanonical: 1, LinesVariant: 1,
			BytesCanonical: 100, BytesVariant: 100,
		})
	}
	var b strings.Builder
	green := WriteReport(&b, results)
	if !green {
		t.Fatalf("expected the report to score this GREEN; it did not:\n%s", b.String())
	}
	if strings.Contains(b.String(), "no canonical origin reached the browser") {
		t.Errorf("the verdict claims an invariant no row in this run tested:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "nothing in this run is a type the proxy rewrites") {
		t.Errorf("the verdict does not say the run scanned nothing:\n%s", b.String())
	}
	t.Logf("report:\n%s", b.String())
}

// TestR45APageTwoLinksDeepStillBeatsTheFirstPagesAssets: why the subresources
// need their own queue and not merely their own ordering within a page.
//
// Separating them in `links` puts a page's own links ahead of its own assets.
// It does not put the *next* page's links ahead of the previous page's assets:
// with one FIFO, `/` enqueues [/b, …30 assets], popping /b appends /c behind
// those assets, and a shallow budget never reaches /c. Draining pages first
// makes depth beat breadth-of-assets, which is what a crawl is for.
func TestR45APageTwoLinksDeepStillBeatsTheFirstPagesAssets(t *testing.T) {
	pages := map[string]string{
		"/":   `<a href="/b/">b</a>`,
		"/b/": `<a href="/c/">c</a>`,
		"/c/": `<p>the page a shallow budget must still reach</p>`,
	}
	// Thirty assets on the first page, all enqueued before /b is popped.
	var head strings.Builder
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&head, `<link rel="stylesheet" href="/a%d.css">`, i)
		pages[fmt.Sprintf("/a%d.css", i)] = "body{}"
	}
	pages["/"] = head.String() + pages["/"]

	results, err := Run(context.Background(), Options{
		Canonical: site(t, pages), Variant: site(t, pages), Map: testMap(t), N: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range results {
		got[r.Path] = true
	}
	for _, want := range []string{"/b/", "/c/"} {
		if !got[want] {
			t.Errorf("a budget of 5 never reached %s; it spent itself on assets: %v", want, got)
		}
	}
}
