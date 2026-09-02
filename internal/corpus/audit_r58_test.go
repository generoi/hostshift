package corpus

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/generoi/hostshift/internal/rewrite"
)

// `hostshift diff` scores a redirect against a narrower pipeline than the proxy
// runs, and the difference is every view urlobf.go exists for.
//
// diff.go computes its expected Location with `Map.Forward().Rewrite(...)` — the
// byte matcher alone. modifyResponse computes the real one with
// `RepairSerialized(Rewrite → HostLeaksCounted)`. So on every spelling the byte
// matcher cannot see, the scorer's `want` is the canonical origin unchanged: the
// correct variant Location the proxy emitted is reported as a mismatch whose
// wanted value is the production URL, on the run the README calls the check that
// validates a deployment against reality and PLAN §7 calls the only one that
// does.
//
// ada: `new URL("https:\\c.example/x", "https://v.example/p").href` is
// `https://c.example/x`, so the header is a dereferenceable production origin
// and the proxy must rewrite it. A scorer that wants it left alone names a test
// 28 leak as the desired state — and by the same narrowness cannot see the
// inverse, because a Location the proxy failed to rewrite is byte-identical on
// both sides *and* equal to this `want`, which scores GREEN.
//
// The same narrowness drops RepairSerialized, so a `Location:
// /landing.php?state=<blob>` whose length the proxy repaired is a mismatch too.
func TestR58DiffScoresTheLocationTheProxyActuallyEmits(t *testing.T) {
	m := testMap(t)

	// An obfuscated absolute Location: `\` is a slash to the URL parser, so this
	// is the canonical origin to a browser and the byte matcher cannot see it.
	const loc = `https:\\c.example/x`

	fwd := m.Forward()
	st := rewrite.NewStats(false)
	// modifyResponse's Tier 1 header expression, verbatim.
	served := string(rewrite.RepairSerialized([]byte(loc), func(b []byte) []byte {
		nv, _ := fwd.Rewrite(b, rewrite.SurfaceHeader, false)
		return rewrite.HostLeaksCounted(fwd, nv, true, st, rewrite.SurfaceHeader, 0)
	}))
	if served == loc {
		t.Fatalf("fixture is not a rewrite: the proxy leaves %q alone", loc)
	}
	if bs, _ := fwd.Rewrite([]byte(loc), rewrite.SurfaceHeader, false); string(bs) != loc {
		t.Fatalf("fixture is not locator-only: the byte matcher already rewrites %q", loc)
	}

	results, err := Run(context.Background(), Options{
		Canonical: r58Redirector(t, loc),
		Variant:   r58Redirector(t, served),
		Map:       m, N: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if !WriteReport(&buf, results) {
		t.Errorf("the scorer red-flagged the Location the proxy actually emits, and "+
			"its `want` is the production origin:\n%s", buf.String())
	}
}

func r58Redirector(t *testing.T, loc string) *url.URL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
