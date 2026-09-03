package corpus

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Round 42, on 94d2cd0 ("stop failing on one dns-prefetch").
//
// The line-count assertion was demoted from fatal to a note. The commit's
// justification is that the delta it saw was exactly one line, every time, from
// a Host-dependent `<link rel="dns-prefetch">`, and that what it stood in for is
// now covered by `broken` and `unread`.
//
// The demotion is total, not bounded: *any* line delta, of any magnitude, is now
// a note. And nothing else in WriteReport turns byte inequality into RED —
// `green = false` is set only by Err, Leaks, UnreadRewrites and BrokenSerialized.
// So after the demotion the scorer has no assertion at all about the *size* of
// what the proxy served, and the two cases below are GREEN.
//
// The report does not merely stay silent. It prints, in the same breath as a row
// saying the line count went 1800→0:
//
//	corpus diff GREEN: no canonical origin reached the browser, no page re-serialised
//
// which is the sentence a developer reads, and it is false: the page it just
// scored lost every line it had. WriteReport's own doc comment still promises
// "no page lost or gained lines", and Result.LinesCanonical's still says "a
// line-count change means something re-serialised".

// emptyBodyVariant: the proxy answers 200 with nothing in it.
//
// Both sides 200, no Location, so the "empty body and no Location: nothing was
// verified" guard does not fire — it requires *both* bodies to be empty, and the
// canonical one is a full page. The variant carries no canonical origin because
// it carries nothing, so Leaks is 0; there is no serialized value to be broken
// or unread. Before 94d2cd0 the 2→0 line count made this RED; the
// unbounded demotion made it GREEN, and the bound restores it.
//
// This is the "hostshift is not in the path at all" shape that the Location
// comparison above it, audit_r40 and audit_r41 were each written to close, and
// it is back through the one door those did not cover.
func TestAnEmptyVariantBodyFailsTheRun(t *testing.T) {
	canonical := map[string]string{
		"/": "<a href=\"" + canonicalOrigin + "/a\">a</a>\n<p>and a second line</p>\n",
	}
	blank := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
	}))
	defer blank.Close()
	variant, err := url.Parse(blank.URL)
	if err != nil {
		t.Fatal(err)
	}

	results, err := Run(t.Context(), Options{
		Canonical: site(t, canonical), Variant: variant, Map: testMap(t), Paths: []string{"/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The premise, so a fixture mistake cannot be mistaken for the finding:
	// the variant really is empty, and the two sides really do disagree.
	if len(results) != 1 {
		t.Fatalf("crawled %d pages, want 1", len(results))
	}
	r := results[0]
	if r.Equal {
		t.Fatal("fixture: the two sides must differ or this tests nothing")
	}
	if r.LinesVariant != 0 || r.LinesCanonical == 0 {
		t.Fatalf("fixture: want a full canonical page and an empty variant, got %d/%d",
			r.LinesCanonical, r.LinesVariant)
	}
	if r.Err != nil {
		t.Fatalf("fixture: no error was expected here, got %v", r.Err)
	}

	var buf bytes.Buffer
	green := WriteReport(&buf, results)
	if green {
		t.Errorf("the proxy served an empty body for every page and the run is GREEN:\n%s",
			buf.String())
	}
	// And the verdict line asserts, in words, the thing it stopped checking.
	if green && strings.Contains(buf.String(), "no page re-serialised") {
		t.Errorf("GREEN claims \"no page re-serialised\" for a page that went %d lines to %d:\n%s",
			r.LinesCanonical, r.LinesVariant, buf.String())
	}
}

// truncatedVariant: the proxy served the first half of the page.
//
// Less extreme than the empty body and harder to notice: a response cut short
// mid-document — an upstream that dies mid-stream, a Content-Length that
// disagrees with the bytes, a rewriter that stops early. The half that survives
// carries no canonical origin, so every other column is clean, and the only
// evidence the scorer ever had that half the page is missing was the line count.
func TestATruncatedVariantFailsTheRun(t *testing.T) {
	head := "<html>\n<head>\n<title>t</title>\n</head>\n<body>\n"
	tail := "<a href=\"" + canonicalOrigin + "/a\">a</a>\n</body>\n</html>\n"
	canonical := map[string]string{"/": head + tail}

	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, head)
	}))
	defer cut.Close()
	variant, err := url.Parse(cut.URL)
	if err != nil {
		t.Fatal(err)
	}

	results, err := Run(t.Context(), Options{
		Canonical: site(t, canonical), Variant: variant, Map: testMap(t), Paths: []string{"/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if r.LinesCanonical == r.LinesVariant {
		t.Fatalf("fixture: the truncation must change the line count, got %d/%d",
			r.LinesCanonical, r.LinesVariant)
	}
	if r.Leaks != 0 || r.BrokenSerialized != 0 || r.UnreadRewrites != 0 || r.Err != nil {
		t.Fatalf("fixture: every other column must be clean, or this does not isolate "+
			"the line count: leaks=%d broken=%d unread=%d err=%v",
			r.Leaks, r.BrokenSerialized, r.UnreadRewrites, r.Err)
	}

	var buf bytes.Buffer
	if WriteReport(&buf, results) {
		t.Errorf("the proxy served %d of %d lines and the run is GREEN:\n%s",
			r.LinesVariant, r.LinesCanonical, buf.String())
	}
}

// TestALongPageLosingATailFailsTheRun: the other half of the bound.
//
// The two cases above both lose a quarter or more of a short document, so the
// proportional half of the bound catches them and the absolute half is never
// exercised. This is the shape that needs the absolute half: a page long enough
// that a lost tail is a small fraction of it. 100 lines down to 88 is 12% — well
// inside the ratio — and no Host-dependent markup produces twelve lines.
func TestALongPageLosingATailFailsTheRun(t *testing.T) {
	var full strings.Builder
	for i := 0; i < 100; i++ {
		full.WriteString("<p>line</p>\n")
	}
	whole, tail := full.String(), strings.Repeat("<p>line</p>\n", 88)

	canonical := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, whole)
	}))
	defer canonical.Close()
	variant := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, tail)
	}))
	defer variant.Close()

	cu, _ := url.Parse(canonical.URL)
	vu, _ := url.Parse(variant.URL)
	results, err := Run(context.Background(), Options{
		Canonical: cu, Variant: vu, Map: testMap(t), N: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Leaks != 0 {
		t.Fatalf("this fixture should leak nothing; it tests line counts")
	}
	var buf bytes.Buffer
	if WriteReport(&buf, results) {
		t.Errorf("the proxy served 88 of 100 lines and the run is GREEN:\n%s", buf.String())
	}
}
