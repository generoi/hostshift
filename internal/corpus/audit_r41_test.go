package corpus

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// redirectsToItself compares an origin against half of one.
//
// PLAN §5.3: "the map is origin→origin (scheme + host + port), never
// host→host". The proxy's guard honours that — normaliseURL runs the Location
// and the request URL through origin.Parse and compares HostPort() — and it
// compares against *the one request the browser made*, st.url.
//
// diff.go:337 asks a different question in both halves:
//
//		for _, site := range m.Sites {
//		    if strings.EqualFold(u.Host, site.Variant.Host) {
//		        return true
//		    }
//		}
//
//	  - `site.Variant.Host` is the hostname with the port deliberately stripped
//	    (origin.Origin keeps Port separately, "" when default), while `u.Host`
//	    is host:port. They can never be equal for a variant with a port, and are
//	    equal for two different origins that share a hostname.
//	  - `m.Sites` is *every* site in the map, not the one being crawled. The
//	    fleet's 12 multisite repos carry N from 2 to 9 (PLAN §"N→N mapping"), so
//	    "some variant in the map" and "the variant the browser is on" routinely
//	    differ.
//
// Each half fails in a different direction, and the tests below are one of
// each.
func r41Map(t *testing.T, sites ...origin.Site) *origin.Map {
	t.Helper()
	m, err := origin.NewMap(sites)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestACrossSiteCanonicalRedirectIsNotSilentlyGreen is the false GREEN.
//
// A two-site network: www.a.fi and www.b.fi, each with its own variant. The
// secondary domain 301s every path to the same path on the primary — the
// ordinary shape of a consolidated or aliased network domain — so a crawl of
// site A's variant answers, on every page:
//
//	HTTP/1.1 301
//	Location: https://www.b.fi/<the path that was asked for>
//
// That is a dereferenceable production origin handed to the browser, which
// follows it: test 28's failure, and the "hostshift is not in the path at all"
// shape audit_r40_test.go was written for.
//
// The proxy does not exempt it. Its guard is sameURL(rewritten, st.url), and
// st.url is "https://" + the Host the browser used — site A's variant — while
// the rewritten Location names site B's. They differ, so modifyResponse
// rewrites the header, which is why a working deployment never produces these
// bytes.
//
// The scorer exempts it anyway: rewriting the Location yields site B's variant
// host, that host is in m.Sites, and the path matches, so redirectsToItself
// returns true and the Location assertion is skipped. Two empty bodies are
// equal, so nothing else objects and WriteReport prints GREEN.
func TestACrossSiteCanonicalRedirectIsNotSilentlyGreen(t *testing.T) {
	mp := r41Map(t,
		origin.Site{
			Name:      "secondary",
			Canonical: origin.MustParse("https://www.a.fi"),
			Variant:   origin.MustParse("https://wt--a.ddev.site"),
		},
		origin.Site{
			Name:      "primary",
			Canonical: origin.MustParse("https://www.b.fi"),
			Variant:   origin.MustParse("https://wt--b.ddev.site"),
		},
	)

	// The mechanical statement first: crawling site A's variant, a Location
	// naming site B's is *not* a self-redirect. It used to be exempted — the
	// exemption accepted any variant in the map rather than the one the browser
	// is on — and that is what let the crawl below go green.
	crawlingA := Options{Map: mp, Variant: &url.URL{Scheme: "https", Host: "wt--a.ddev.site"}}
	if redirectsToItself("https://wt--b.ddev.site/kotiasiakkaille/", crawlingA, "/kotiasiakkaille/") {
		t.Fatal("a redirect to another site's variant is still exempted")
	}
	// And site A's own self-redirect still is, so this is not a blanket refusal.
	if !redirectsToItself("https://wt--a.ddev.site/kotiasiakkaille/", crawlingA, "/kotiasiakkaille/") {
		t.Fatal("the crawled site's own self-redirect stopped being exempted")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Same path, other host. Not a self-redirect by PLAN §4.4's definition
		// — rewriting it does not yield the URL the browser asked for, because
		// the browser is on site A's variant and this names site B's.
		w.Header().Set("Location", "https://www.b.fi"+r.URL.EscapedPath())
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer srv.Close()

	// Two parses: fetch() tells the sides apart by pointer, and this models
	// --variant-base pointed at a deployment hostshift is not in front of.
	canon, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	variant, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	results, err := Run(t.Context(), Options{
		Map:       mp,
		Canonical: canon,
		Variant:   variant,
		Paths:     []string{"/", "/kotiasiakkaille/", "/yhteystiedot/"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if WriteReport(&out, results) {
		t.Fatalf("every page handed the browser a Location on https://www.b.fi — a "+
			"production origin in the map, at a URL the proxy's own guard would "+
			"have rewritten — and the run is GREEN:\n%s", out.String())
	}
}

// TestTheSelfRedirectExemptionSurvivesAPortedVariant is the false RED, and the
// same line.
//
// `hostshift proxy --listen 127.0.0.1:8080` outside DDEV is a documented mode
// (PLAN §"hostshift proxy --upstream http://web:80 --listen 127.0.0.1:8080"),
// and `variant: http://127.0.0.1:8080` is a config the loader accepts and
// config_test.go asserts. On such a map the browser is on localhost:8080, so
// the proxy's guard exempts redirect-uploads exactly as §4.4 says:
// normaliseURL puts both sides through origin.HostPort(), which keeps the
// port, and "localhost:8080/app/uploads/x.jpg" == "localhost:8080/app/uploads/x.jpg".
//
// The scorer compares u.Host ("localhost:8080") against site.Variant.Host
// ("localhost" — Origin keeps the port in a separate field), so the exemption
// can never fire. 87% of the fleet ships redirect-uploads.conf and 95.2% of
// referenced uploads are absent locally, so every page linking an upload turns
// the run RED on a deployment doing exactly what PLAN §4.4 prescribes — which
// is the outcome diff.go's own comment says the carve-out exists to prevent.
func TestTheSelfRedirectExemptionSurvivesAPortedVariant(t *testing.T) {
	const path = "/app/uploads/2025/07/x.jpg"
	mp := r41Map(t, origin.Site{
		Name:      "main",
		Canonical: origin.MustParse("https://www.acme.fi"),
		Variant:   origin.MustParse("http://localhost:8080"),
	})

	// What the proxy sees on the wire, spelled out: the raw Location, and what
	// the forward map turns it into.
	loc := "https://www.acme.fi" + path
	rewritten, _ := mp.Forward().Rewrite([]byte(loc), "header", false)
	if got, want := string(rewritten), "http://localhost:8080"+path; got != want {
		t.Fatalf("the premise no longer holds: rewrite(%q) = %q, want %q", loc, got, want)
	}
	// The proxy's sameURL(rewritten, st.url) with st.url = "https://" + Host +
	// RequestURI. origin.HostPort() keeps the port on both sides, so this is
	// true and the proxy passes the Location through per §4.4 and test 32.
	ported := Options{Map: mp, Variant: &url.URL{Scheme: "http", Host: "localhost:8080"}}
	if !redirectsToItself(string(rewritten), ported, path) {
		t.Fatalf("the proxy exempts %q for a request to %q (sameURL normalises both "+
			"through origin.HostPort, which keeps :8080), and the scorer does not — "+
			"so every page linking an upload is RED on a healthy deployment",
			rewritten, path)
	}

	// And end to end, because a verdict is what a developer reads.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://www.acme.fi"+r.URL.EscapedPath())
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	cl := srv.Client()
	cl.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	r := compare(t.Context(), Options{
		Map: mp, Canonical: base, Variant: base, Client: cl,
	}, path)
	if r.Err != nil && strings.Contains(r.Err.Error(), "Location") {
		t.Errorf("a documented carve-out reported as an error on a ported variant: %v", r.Err)
	}
}
