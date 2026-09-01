package corpus

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 47.
//
// The engine cannot read a JSON `\uXXXX` escape of a non-ASCII host — see
// internal/rewrite's TestR47AnIDNCanonicalEscapedByWPJSONEncodeReachesTheBrowser
// for why that spelling is what wp_json_encode emits on every page of a site
// with an IDN canonical. This is what the corpus diff says about such a page.
//
// originsIn "asks the whole engine, not the byte matcher alone", and its comment
// explains that asking the matcher alone made every leak class the engine could
// not see invisible to the one test §7 calls the only one that validates against
// reality. The engine is a better oracle than the matcher, and it is still the
// engine: a spelling the engine cannot read is a spelling this cannot count. So
// the page below is served to the browser with a live production origin in it,
// the diff reads it byte for byte, and prints GREEN.
func TestR47TheDiffIsGreenOnAPageThatLeaksAnEscapedIDN(t *testing.T) {
	const canon = "https://www.hämeenlinna.fi"
	// The six bytes wp_json_encode writes for the `ä`.
	const escaped = "www.h" + "\\u00e4" + "meenlinna.fi"

	m, err := origin.NewMap([]origin.Site{{
		Name:      "hml",
		Canonical: origin.MustParse(canon),
		Variant:   origin.MustParse("https://wt-a--hml.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	page := `<html><body><script>var w={"ajax":"https:\/\/` + escaped +
		`\/wp-admin\/admin-ajax.php"};</script></body></html>`
	pages := map[string]string{"/": page}

	// The proxy changed nothing, because it could not see anything — so the
	// variant response is the canonical bytes verbatim.
	var canonURL, variantURL *url.URL
	canonURL = site(t, pages)
	variantURL = site(t, pages)

	results, err := Run(context.Background(), Options{
		Canonical: canonURL, Variant: variantURL, Map: m, N: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("crawled %d pages, want 1", len(results))
	}

	var buf bytes.Buffer
	green := WriteReport(&buf, results)
	if results[0].Leaks == 0 {
		t.Errorf("the production origin in this page was not counted as a leak; "+
			"the browser resolves it to %s/wp-admin/admin-ajax.php:\n%s", canon, page)
	}
	if green {
		t.Errorf("and the run is GREEN, so the check the README calls "+
			"\"the check that validates a deployment against reality\" signs off on it:\n%s",
			buf.String())
	}
	if strings.Contains(buf.String(), "CANONICAL ORIGIN REACHED THE BROWSER") {
		t.Logf("report named the failure:\n%s", buf.String())
	}
}
