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

// The corpus diff's self-redirect carve-out is not the proxy's.
//
// PLAN §4.4 defines the guard exactly: "if rewriting `Location` canonical→variant
// would yield a URL equal to the incoming request URL, emit the `Location`
// unmodified". proxy.modifyResponse implements that — it calls sameURL against
// st.url, the request the browser made — and test 32 spells out the other half:
// "assert that a 3xx whose `Location` differs from the incoming request URL is
// still rewritten normally (a login redirect, test 1, must not be caught by the
// guard)".
//
// compare() in diff.go asks a much weaker question:
//
//	unchangedSelfRedirect := !o.StrictOrigins &&
//	    variant.location == canon.location && canon.location != ""
//
// There is no comparison against the path being fetched, so *any* Location the
// variant returned unchanged is exempted — not just the one that would loop. A
// canonical Location that the deployment failed to rewrite is by construction
// identical on both sides, which is the exact condition that switches the check
// off.
//
// The two tests below are the two halves of that: the shape PLAN says must be
// rewritten normally, and the whole-deployment failure the Location comparison
// was added to catch in the first place.
func r40Map(t *testing.T) *origin.Map {
	t.Helper()
	m, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://acme.ddev.site"),
		Variant:   origin.MustParse("https://wt-a--acme.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestALoginRedirectLeftUnrewrittenIsNotGreen is test 32's second half, asked of
// the scorer instead of the proxy.
//
// The request is for /wp-admin/ and the Location is /wp-login.php?redirect_to=…
// on the canonical origin — a different URL from the one asked for, so the
// self-redirect guard does not apply to it and the proxy rewrites it. A
// deployment that serves it unchanged sends the developer's browser to the
// production login form, which is a dereferenceable production origin reaching
// the browser: test 28.
//
// The scorer sees the same bytes on both sides and calls it a carve-out.
func TestALoginRedirectLeftUnrewrittenIsNotGreen(t *testing.T) {
	const (
		reqPath = "/wp-admin/"
		// Deliberately not the request URL: this is the login redirect PLAN
		// test 32 names, not the redirect-uploads self-redirect.
		leaked = "https://acme.ddev.site/wp-login.php?redirect_to=%2Fwp-admin%2F"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", leaked)
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
		Map: r40Map(t), Canonical: base, Variant: base, Client: cl,
	}, reqPath)

	if r.Err == nil {
		t.Fatalf("the variant served %q for a request to %q — a canonical origin the "+
			"browser follows, and a URL the self-redirect guard does not cover — "+
			"and the scorer reported no error, so the run is GREEN: %+v",
			leaked, reqPath, r)
	}
	if !strings.Contains(r.Err.Error(), "Location") {
		t.Errorf("the reason does not name the header: %v", r.Err)
	}
}

// TestAnAllRedirectCrawlWithHostshiftOutOfThePathIsNotGreen is the failure the
// Location comparison exists for, verbatim from diff.go's own comment:
//
//	"Comparing bodies alone scored an all-redirect crawl as '3 pages, 3
//	byte-identical, 0 leaks — GREEN' with hostshift not in the path at all,
//	because two empty bodies are equal. The shapes that produce such a crawl
//	are the documented ones: a worktree whose database is empty redirects every
//	page to install.php…"
//
// So: a worktree with an empty database, and --variant-base pointing at the
// canonical site rather than at the proxy. Every page 302s to install.php on
// the canonical origin, the two sides are byte-identical because they are the
// same server, and the carve-out cancels the one assertion that could have
// noticed. The report says "no canonical origin reached the browser" while
// every page hands the browser a production URL.
func TestAnAllRedirectCrawlWithHostshiftOutOfThePathIsNotGreen(t *testing.T) {
	const install = "https://acme.ddev.site/wp-admin/install.php"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", install)
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	// Two parses, because this models a *misconfigured* --variant-base: the
	// flag was pointed at the canonical site, so hostshift is nowhere in the
	// path. fetch() distinguishes the two sides by pointer.
	canon, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	variant, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	results, err := Run(t.Context(), Options{
		Map:       r40Map(t),
		Canonical: canon,
		Variant:   variant,
		Paths:     []string{"/", "/kotiasiakkaille/", "/yhteystiedot/"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	green := WriteReport(&out, results)
	if green {
		t.Fatalf("every page served %q and the run is GREEN — hostshift is not in "+
			"the path at all:\n%s", install, out.String())
	}
}

// The self-redirect exemption needs both halves: the same path, and a host the
// map actually names as a variant.
//
// PLAN §4.4 defines the guard as "rewriting this Location would yield the URL
// the browser just requested". Both halves of that matter, and neither is
// implied by the other: a redirect to a *different* path on the right host is
// an ordinary redirect that must be rewritten (test 32's login case), and a
// redirect to the same path on someone else's host is not this deployment's
// redirect at all.
func TestTheSelfRedirectExemptionNeedsPathAndHost(t *testing.T) {
	mp, err := origin.NewMap([]origin.Site{{
		Name: "main", Canonical: origin.MustParse("https://acme.ddev.site"),
		Variant: origin.MustParse("https://wt-a--acme.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		loc  string
		want bool
	}{
		"the same path on a variant host": {"https://wt-a--acme.ddev.site/a", true},
		"a different path":                {"https://wt-a--acme.ddev.site/b", false},
		"a different query":               {"https://wt-a--acme.ddev.site/a?x=1", false},
		"a third-party host":              {"https://cdn.example.net/a", false},
		"the canonical host":              {"https://acme.ddev.site/a", false},
	} {
		// Options rather than the bare map: the exemption is about *the* variant
		// being crawled, so it needs to know which one that is.
		o := Options{Map: mp, Variant: &url.URL{Scheme: "https", Host: "wt-a--acme.ddev.site"}}
		if got := redirectsToItself(tc.loc, o, "/a"); got != tc.want {
			t.Errorf("%s: redirectsToItself(%q) = %v, want %v", name, tc.loc, got, tc.want)
		}
	}
}
