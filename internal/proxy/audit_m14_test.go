package proxy

import (
	"net/http"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// An identity map is a no-op on the *response*, not only on the body.
//
// That premise is this codebase's own, stated twice. transport.go's
// compressBody returns early under `p.Map.Identity()` because "an identity map
// must be a no-op end to end, not merely in the body (test 24)". finishBody
// refuses to set `changed` for the same reason: "an identity map cannot change
// a byte, so the upstream's length and validators still describe the body.
// Dropping them anyway made test 24's premise — that an identity map is a
// no-op — true of the body but not of the response."
//
// Two things reach the response ahead of that guard and carry no guard of their
// own. dropCookieDomain strips a `Domain=` naming a canonical host — and under
// an identity map every variant host *is* its own canonical, so the attribute
// always matches. That also sets `changed`, which then walks straight into the
// branch finishBody's comment exists to prevent: ETag, Last-Modified,
// Content-Length and Accept-Ranges are dropped from a body nothing touched.
// And addVary appends `Vary: Host` to a response whose headers were not
// rewritten at all.
//
// The cookie change is not cosmetic. `ms_cookie_constants()` sets
// COOKIE_DOMAIN from the network domain on a subdomain multisite, and dropping
// the attribute makes the cookie host-only — which is the right answer on a
// variant host and the wrong one when the proxy was asked to be a mirror.
func TestAnIdentityMapDoesNotTouchTheResponseHeaders(t *testing.T) {
	o := origin.MustParse("https://acme.test")
	m, err := origin.NewMap([]origin.Site{{Name: "acme", Canonical: o, Variant: o}})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Identity() {
		t.Fatalf("the fixture is not an identity map, so this asserts nothing")
	}
	const (
		testCookie = "wordpress_test_cookie=WP+Cookie+check; path=/; domain=.acme.test; secure"
		settings   = "wp-settings-1=x; path=/; domain=acme.test"
		etag       = `"abc"`
		modified   = "Wed, 21 Oct 2026 07:28:00 GMT"
	)
	hs := newHarness(t, m, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", testCookie)
		w.Header().Add("Set-Cookie", settings)
		w.Header().Set("ETag", etag)
		w.Header().Set("Last-Modified", modified)
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<p>hi</p>"))
	})
	resp, body := hs.get(t, "acme.test", "/")

	// The control: if the body were altered the invariant would already be
	// failing for a reason that has nothing to do with headers.
	if string(body) != "<p>hi</p>" {
		t.Fatalf("the identity map altered the body, so this asserts nothing: %q", body)
	}

	t.Run("Set-Cookie keeps its Domain", func(t *testing.T) {
		got := resp.Header.Values("Set-Cookie")
		want := []string{testCookie, settings}
		if len(got) != len(want) {
			t.Fatalf("got %q want %q", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("a proxy asked to be a no-op made the cookie host-only:\n got  %q\n want %q",
					got[i], want[i])
			}
		}
	})

	t.Run("the validators still describe the body", func(t *testing.T) {
		for _, c := range []struct{ name, want string }{
			{"ETag", etag},
			{"Last-Modified", modified},
		} {
			if got := resp.Header.Get(c.name); got != c.want {
				t.Errorf("%s was dropped from a byte-identical body:\n got  %q\n want %q",
					c.name, got, c.want)
			}
		}
	})

	t.Run("no header is added", func(t *testing.T) {
		if got := resp.Header.Values("Vary"); len(got) != 0 {
			t.Errorf("the upstream sent no Vary and the response carries %q", got)
		}
	})
}
