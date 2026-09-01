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

// Round 43, on e37c0b0 ("Bound the line-count check instead of deleting it").
//
// The bound is expressed entirely in newlines: `strings.Count(body, "\n")` on
// each side, then `d > hostDependentLines || d*4 > r.LinesCanonical`. A document
// that contains no newlines has a line count of 0 whatever is in it — and 0 is
// also the line count of nothing at all. So for that whole class of document the
// two counts are equal by construction, the bound is never consulted, and the
// scorer is back to having no assertion about the size of what the proxy served.
//
// The class is not exotic. It is every minified page — WP Rocket, Autoptimize,
// LiteSpeed Cache and Cloudflare's own minifier all emit one line — and every
// JSON body, which `--paths` reaches routinely.
//
// r42's own TestAnEmptyVariantBodyFailsTheRun is the same fixture with two
// newlines in the canonical page. Remove them and the run is GREEN again, under
// the verdict line the same commit extended to say "every page the length it
// should be".

// TestAMinifiedPageTruncatedToNothingFailsTheRun: the canonical page is a whole
// document on one line; the variant answers 200 with nothing. Both sides 200 and
// no Location, so the "empty body and no Location" guard does not fire — it
// needs *both* bodies empty. Nothing reached the browser, so Leaks is 0; there
// is no serialized value to be broken or unread. The only thing that could
// notice is the size bound, and newlines are the only unit it can count in.
func TestAMinifiedPageTruncatedToNothingFailsTheRun(t *testing.T) {
	// One line, no trailing newline: what an HTML minifier serves.
	page := "<!doctype html><html><head><title>Acme</title></head><body>" +
		"<a href=\"" + canonicalOrigin + "/a\">a</a>" +
		"<p>the whole document, on one line, as a minifier emits it</p>" +
		"</body></html>"
	if strings.Contains(page, "\n") {
		t.Fatal("fixture: the canonical page must have no newline in it")
	}
	canonical := map[string]string{"/": page}

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
	if len(results) != 1 {
		t.Fatalf("crawled %d pages, want 1", len(results))
	}
	r := results[0]
	// The premise, stated so that a fixture mistake cannot be read as the
	// finding: the variant really is empty, the canonical really is a page, and
	// the two really do disagree.
	if r.Err != nil {
		t.Fatalf("fixture: %v", r.Err)
	}
	if r.Equal {
		t.Fatal("fixture: the two sides must differ or this tests nothing")
	}
	if r.Leaks != 0 || r.BrokenSerialized != 0 || r.UnreadRewrites != 0 {
		t.Fatalf("fixture: this must be a page nothing else in the report objects to, got "+
			"leaks=%d broken=%d unread=%d", r.Leaks, r.BrokenSerialized, r.UnreadRewrites)
	}
	if r.LinesCanonical != 0 || r.LinesVariant != 0 {
		t.Fatalf("fixture: a newline-free page and an empty body both count 0 lines, got %d/%d",
			r.LinesCanonical, r.LinesVariant)
	}

	var buf bytes.Buffer
	if WriteReport(&buf, results) {
		t.Errorf("the proxy served an empty body for a whole page and the run is GREEN:\n%s",
			buf.String())
	}
}

// TestAMinifiedPageServedInHalfFailsTheRun is the truncation shape rather than
// the empty one: the upstream dies mid-stream and the browser gets the first
// third of the document. On a one-line page that is still 0 lines against 0
// lines.
func TestAMinifiedPageServedInHalfFailsTheRun(t *testing.T) {
	page := "<!doctype html><html><head><title>Acme</title></head><body>" +
		"<a href=\"" + canonicalOrigin + "/a\">a</a>" +
		strings.Repeat("<p>body copy that the browser never receives</p>", 40) +
		"</body></html>"
	cut := page[:60]
	if strings.Contains(page, "\n") {
		t.Fatal("fixture: the canonical page must have no newline in it")
	}

	half := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(cut))
	}))
	defer half.Close()
	variant, err := url.Parse(half.URL)
	if err != nil {
		t.Fatal(err)
	}

	results, err := Run(t.Context(), Options{
		Canonical: site(t, map[string]string{"/": page}), Variant: variant,
		Map: testMap(t), Paths: []string{"/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := results[0]
	if r.Err != nil {
		t.Fatalf("fixture: %v", r.Err)
	}
	if r.Equal {
		t.Fatal("fixture: the two sides must differ or this tests nothing")
	}
	// The premise: the browser received under a tenth of the document.
	if len(cut)*10 > len(page) {
		t.Fatalf("fixture: %d of %d bytes is not a truncation", len(cut), len(page))
	}
	if r.LinesCanonical != r.LinesVariant {
		t.Fatalf("fixture: both sides must count 0 lines, got %d/%d",
			r.LinesCanonical, r.LinesVariant)
	}

	var buf bytes.Buffer
	if WriteReport(&buf, results) {
		t.Errorf("the proxy served %d bytes of a %d-byte page and the run is GREEN:\n%s",
			len(cut), len(page), buf.String())
	}
}

// TestTheVerdictDoesNotClaimWhatTier2SkipsWasChecked: a production-canonical run
// with live origins inside Elementor CSS printed, two lines apart:
//
//	4 origins in Tier 2 types (text/css, JavaScript), which the proxy excludes…
//	corpus diff GREEN: no canonical origin reached the browser, …
//
// and exited 0. The exclusion is designed and stays; the sentence asserting the
// one thing invariant 28 forbids, about bytes the run never looked at, cannot —
// this is the command the README calls the check that validates a deployment
// against reality, and anything gating on its exit status was guarding nothing.
func TestTheVerdictDoesNotClaimWhatTier2SkipsWasChecked(t *testing.T) {
	css := `body{background:url(` + canonicalOrigin + `/bg.png)}` + "\n"
	page := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		io.WriteString(w, css)
	}
	canonical := httptest.NewServer(http.HandlerFunc(page))
	defer canonical.Close()
	variant := httptest.NewServer(http.HandlerFunc(page))
	defer variant.Close()

	cu, _ := url.Parse(canonical.URL)
	vu, _ := url.Parse(variant.URL)
	results, err := Run(context.Background(), Options{
		Canonical: cu, Variant: vu, Map: testMap(t), N: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	green := WriteReport(&buf, results)
	if results[0].Tier2 == 0 {
		t.Fatalf("fixture: this page should carry a Tier 2 origin:\n%s", buf.String())
	}
	// Still green — the exclusion is a decision, not a defect.
	if !green {
		t.Fatalf("Tier 2 must not fail the run; that is PLAN §5.2:\n%s", buf.String())
	}
	// But the sentence must not assert what it skipped.
	out := buf.String()
	if strings.Contains(out, "GREEN: no canonical origin reached the browser") {
		t.Errorf("the verdict claims Tier 2 bytes were checked:\n%s", out)
	}
	if !strings.Contains(out, "did reach it in Tier 2") {
		t.Errorf("the verdict does not say origins reached the browser:\n%s", out)
	}
}
