package corpus

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// diff compared bodies only, so an all-redirect crawl scored "byte-identical,
// 0 leaks, GREEN" with hostshift not in the path at all — two empty bodies are
// equal. The shapes that produce such a crawl are the documented ones: a
// worktree whose database is empty redirects every page to install.php, and a
// login-walled preview does the same. §7 calls this the only test that
// validates against reality, and Location is the header §4.4 worries about
// most.
func TestRedirectsAreNotSilentlyGreen(t *testing.T) {
	mp, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://acme.ddev.site"),
		Variant:   origin.MustParse("https://wt-a--acme.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name, location string
		wantErr        string
	}{
		{
			// A genuine mismatch: the two sides redirect somewhere different, so
			// the rewritten canonical Location is not what the variant sent.
			// Distinct from the carve-out below, where both sides send the same
			// canonical Location and the guard is letting it through on purpose.
			"a mismatched Location", "MISMATCH", "Location",
		},
		{
			// Nothing to verify at all.
			"an empty body and no Location", "",
			"nothing was verified",
		},
		{
			// The self-redirect carve-out PLAN §4.4 and test 32 enumerate as
			// correct: an asset the worktree does not have is redirected to the
			// canonical origin on purpose. 87% of the fleet ships
			// redirect-uploads.conf and 95.2% of referenced uploads are absent
			// locally, so flagging this made RED the ordinary outcome.
			"an unchanged self-redirect", "SELF", "",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var reqs atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				loc := c.location
				if loc == "MISMATCH" {
					// The canonical fetch comes first; the variant then gets a
					// different target, which is what a broken deployment looks
					// like.
					n := reqs.Add(1)
					if n == 1 {
						loc = "https://acme.ddev.site/one"
					} else {
						loc = "https://acme.ddev.site/two"
					}
				}
				if loc == "SELF" {
					// The canonical origin, at *the path that was requested*.
					// That is what redirect-uploads.conf does and what PLAN
					// §4.4 defines the guard as: rewriting this Location would
					// yield the URL the browser just asked for.
					//
					// The fixture used to name a different path from the one it
					// requested, which is not a self-redirect at all — so it
					// passed under a guard that exempted *any* unchanged
					// Location, and nothing noticed that guard was wider than
					// the proxy's. A URL-level version of the same-byte-length
					// fixture mistake.
					loc = "https://acme.ddev.site" + r.URL.EscapedPath()
				}
				if loc != "" {
					w.Header().Set("Location", loc)
					w.WriteHeader(http.StatusFound)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			base, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}

			// Not following redirects is the point: a 302 is the response under
			// test, not a step on the way to one.
			cl := srv.Client()
			cl.CheckRedirect = func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			}
			o := Options{
				Map:       mp,
				Canonical: base,
				Variant:   base,
				Client:    cl,
			}
			r := compare(t.Context(), o, "/a")
			if c.wantErr == "" {
				if r.Err != nil {
					t.Fatalf("a documented carve-out was reported as an error: %v", r.Err)
				}
				return
			}
			if r.Err == nil {
				t.Fatalf("reported no error, so a crawl of these would be GREEN: %+v", r)
			}
			if !strings.Contains(r.Err.Error(), c.wantErr) {
				t.Errorf("the reason is not stated: %v", r.Err)
			}
		})
	}
}
