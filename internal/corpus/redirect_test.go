package corpus

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
			// No proxy in the path: the variant's Location still names the
			// canonical, which is precisely what diff exists to notice.
			"an unrewritten Location", "https://acme.ddev.site/wp-signup.php",
			"Location",
		},
		{
			// Nothing to verify at all.
			"an empty body and no Location", "",
			"nothing was verified",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.location != "" {
					w.Header().Set("Location", c.location)
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
			if r.Err == nil {
				t.Fatalf("reported no error, so a crawl of these would be GREEN: %+v", r)
			}
			if !strings.Contains(r.Err.Error(), c.wantErr) {
				t.Errorf("the reason is not stated: %v", r.Err)
			}
		})
	}
}
