package rewrite

import (
	"encoding/base64"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// r69probe builds the request-direction matcher for one canonical/variant pair.
// The reverse matcher rewrites Canonical->Variant, so the request direction is
// the pair with the variant origin in the Canonical slot — exactly what
// origin.NewMap builds for Map.Reverse().
func r69probe(t *testing.T) *origin.Matcher {
	t.Helper()
	c, err := origin.Parse("https://www.r69a.example")
	if err != nil {
		t.Fatal(err)
	}
	v, err := origin.Parse("https://wt-a--r69w.ddev.site")
	if err != nil {
		t.Fatal(err)
	}
	m, err := origin.NewMatcher([]origin.Pair{{Canonical: v, Variant: c}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestR69Base64RunSwallowsItsNeighbour: round 68 made a base64 run
// whitespace-tolerant so that an RFC 2045 line-wrapped blob is one run. The run
// is greedy in both directions, and stripB64Space then concatenates whatever it
// swallowed, so a blob with an ordinary word beside it decodes to nothing and
// the detector — whose whole purpose is to stop being silent about a variant
// hostname going into the shared database — says nothing.
//
// The bare blob is reported. The same blob with one space and one word after it
// is not.
func TestR69Base64RunSwallowsItsNeighbour(t *testing.T) {
	rev := r69probe(t)
	rw := func(b []byte) []byte {
		out, _ := rev.Rewrite(b, SurfaceRequestBody, false)
		return out
	}
	blob := base64.StdEncoding.EncodeToString(
		[]byte(`a:1:{s:5:"title";s:31:"https://wt-a--r69w.ddev.site/x/";}`))

	alone, _ := HiddenInBase64([]byte("value="+blob), rw)
	if alone == 0 {
		t.Fatalf("fixture is wrong: the bare blob was not detected")
	}

	for _, tail := range []string{" note", " x", "\nnote", " abc"} {
		n, _ := HiddenInBase64([]byte("value="+blob+tail), rw)
		t.Logf("blob + %q -> blobs=%d", tail, n)
		if n == 0 {
			t.Errorf("a variant hostname inside base64 went unreported because the "+
				"run swallowed %q; the same blob alone reports %d", tail, alone)
		}
	}
}
