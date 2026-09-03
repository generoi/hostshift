package corpus

import (
	"bytes"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// TestR74CountLeaksIsBlindWhereTheEngineIs shows the amplifier directly, on the
// bytes a running proxy actually served.
//
// countLeaks scores a served body by pushing it back through the same pipeline
// the proxy ran on it. That is right for every origin the engine can see, and
// silent for every origin it cannot: an origin the engine declines going out is
// declined again coming in, and the page scores GREEN. Every engine defect is
// therefore also a hole in "the only test that validates against reality".
//
// The body below is stock WordPress 7.1's `wp-admin/site-health.php?tab=debug`,
// reduced to the two lines that matter: the copy-to-clipboard report is one
// attribute value whose lines are separated by raw LF, and a bare origin before
// one of those is not rewritten (see
// proxy.TestR74BareOriginBeforeAControlInAnAttributeIsNotRewritten). Measured
// against the real thing: `hostshift diff` printed
//
//	/r74attr.php  same  0  17/17
//	corpus diff GREEN: no canonical origin reached the browser
//
// on a page whose served bytes hold `https://www.hostshift-a.example` twice.
//
// A plain `bytes.Contains` of the canonical host is the check that does not
// have that property, because it shares no code with the thing it is checking.
// The cost is false positives on origins the proxy declines on purpose — a bare
// hostname in prose, which the wp-admin sweep below did hit once. Anchoring on
// `//host` rather than `host` removes those: over 65 real pages of a WordPress
// multisite served through the proxy (front end, the whole of wp-admin, network
// admin, REST, feeds, sitemaps) the anchored form fired exactly once, on this
// true positive, and never otherwise.
func TestR74CountLeaksIsBlindWhereTheEngineIs(t *testing.T) {
	c, err := origin.Parse("https://www.r74a.example")
	if err != nil {
		t.Fatal(err)
	}
	v, err := origin.Parse("https://wt-a--r74w.ddev.site")
	if err != nil {
		t.Fatal(err)
	}
	m, err := origin.NewMap([]origin.Site{{Name: "s", Canonical: c, Variant: v}})
	if err != nil {
		t.Fatal(err)
	}

	// Exactly what the proxy served: the table value rewritten, the attribute
	// value not.
	served := []byte(`<!doctype html><html><body>` +
		`<table><tr><th>WP_HOME</th><td>https://wt-a--r74w.ddev.site</td></tr></table>` +
		`<button class="copy-button" data-clipboard-text="### wp-constants ###

WP_HOME: https://www.r74a.example
WP_SITEURL: https://www.r74a.example
WP_CONTENT_DIR: /var/www/html/web/wp-content"></button></body></html>`)

	// The independent check, in one line.
	independent := bytes.Count(served, []byte("//www.r74a.example"))
	if independent == 0 {
		t.Fatal("harness: the fixture carries no canonical origin at all")
	}

	leaks, tier2 := countLeaks(m.Forward(), response{
		body: served, status: 200, contentType: "text/html; charset=UTF-8",
	})
	if tier2 != 0 {
		t.Fatalf("harness: text/html is not a Tier 2 type, got tier2=%d", tier2)
	}
	// countLeaks re-runs the engine, so it cannot see what the engine declines —
	// that is the property being recorded, not a bug to assert away. This test
	// asserted `leaks != 0` when it was written; the remedy shipped is the one
	// this round's report recommended instead, a separate counter, because a
	// literal scan also sees origins PLAN says to leave alone and folding it into
	// `Leaks` would make every Tier 2 page RED.
	if leaks != 0 {
		t.Logf("countLeaks now finds %d here; the independent check is still the "+
			"one that cannot inherit an engine mistake", leaks)
	}
	if got := literalOrigins(m, served); got != independent {
		t.Errorf("the independent check found %d canonical origins, the fixture "+
			"has %d — it is the only witness that does not share code with the "+
			"engine, so it has to see them", got, independent)
	}
}
