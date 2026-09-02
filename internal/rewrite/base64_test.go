package rewrite

import (
	"encoding/base64"
	"strconv"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// The Customizer posts a widget instance as base64 of a serialized array, and
// validates it with `wp_hash()` over exactly those bytes. So this must be
// *reported* and not rewritten: round 66's audit reposted a save with the blob
// correctly rewritten and WordPress answered `invalid_value` and dropped it.
//
// What was wrong was the silence. The save went through, production's database
// took the worktree's hostname, the canonical front page served it to the
// public, nothing logged a line, and `hostshift diff` printed GREEN.
func TestHiddenInBase64(t *testing.T) {
	rev, err := origin.NewMatcher([]origin.Pair{{
		Canonical: origin.MustParse("https://wt-a--example.ddev.site"),
		Variant:   origin.MustParse("https://www.example.fi"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	rw := func(b []byte) []byte {
		out, _ := rev.Rewrite(b, SurfaceRequestBody, false)
		return out
	}
	blob := func(v string) string {
		s := `a:1:{s:7:"content";s:` + strconv.Itoa(len(v)) + `:"` + v + `";}`
		return base64.StdEncoding.EncodeToString([]byte(s))
	}
	carrying := blob(`<a href="https://wt-a--example.ddev.site/promo/">the promo</a>`)
	clean := blob(`<a href="/promo/">the promo</a>`)

	for _, c := range []struct {
		name string
		body string
		want int
	}{
		{"a widget instance carrying a variant hostname",
			"action=customize_save&customized=" + carrying, 1},
		{"the same instance with no origin in it", "customized=" + clean, 0},
		{"two blobs", "a=" + carrying + "&b=" + carrying, 2},
		{"a body with no base64 at all",
			"customized=https%3A%2F%2Fwt-a--example.ddev.site%2Fx", 0},
		// The false-positive guard: base64-looking runs are everywhere in a
		// WordPress body — nonces, ids, digests — and none of them decode to
		// something this map rewrites.
		{"a nonce-shaped run", "_wpnonce=a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0", 0},
		{"a long hex digest",
			"h=9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", 0},
		{"a short run that happens to decode", "x=aGk=", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, sample := HiddenInBase64([]byte(c.body), rw)
			if n != c.want {
				t.Errorf("reported %d blobs, want %d — a save that reaches the shared "+
					"database has to say so\n  body: %s\n  sample: %s",
					n, c.want, c.body, sample)
			}
			if c.want > 0 && len(sample) == 0 {
				t.Error("reported a blob with no sample, so the log names nothing")
			}
		})
	}
}
