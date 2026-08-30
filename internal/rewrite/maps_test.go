package rewrite

import (
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Everything else in this package tests one map: portless, single-scheme,
// canonical https to variant https. That is the ddev case and it is the only
// shape the corpus varies against, so the whole *map* dimension went
// unexercised — and the locator turned out to splice the host alone, dropping
// the variant's scheme and port. The plain spelling was right and every
// obfuscated spelling was wrong, on a configuration `check` calls injective and
// anchored.
//
// Worse than wrong output: the reverse table is keyed host:port, so the request
// direction parsed a portless host, missed, and sent the *variant* hostname
// upstream into the database §4.3 says stays byte-identical to production.
func TestMapShapes(t *testing.T) {
	shapes := []string{
		`https://%s/x`,
		`https:\\%s/x`,
		`https:///%s/x`,
		`https://u@%s/x`,
		`//%s/x`,
	}

	for _, c := range []struct {
		name, canonical, variant string
	}{
		{"variant with a port", "https://www.example.fi", "http://localhost:8080"},
		{"variant on the other scheme", "https://www.example.fi", "http://v.ddev.site"},
		{"canonical with a port", "https://www.example.fi:8443", "https://v.ddev.site"},
		{"both with ports", "https://www.example.fi:8443", "http://localhost:8080"},
		{"an IPv6 variant", "https://www.example.fi", "http://[::1]:8080"},
		{"the ordinary ddev shape", "https://www.example.fi", "https://wt-a--example.ddev.site"},
	} {
		t.Run(c.name, func(t *testing.T) {
			canon := origin.MustParse(c.canonical)
			variant := origin.MustParse(c.variant)
			fwd, err := origin.NewMatcher([]origin.Pair{{Canonical: canon, Variant: variant}})
			if err != nil {
				t.Fatal(err)
			}
			rev, err := origin.NewMatcher([]origin.Pair{{Canonical: variant, Variant: canon}})
			if err != nil {
				t.Fatal(err)
			}

			for _, sh := range shapes {
				host := canon.HostPort()
				in := `<a href="` + strings.Replace(sh, "%s", host, 1) + `">x</a>`
				out := rewriteHTML(t, fwd, in, NewStats(false))

				// Nothing may still name the canonical host.
				if strings.Contains(out, canon.Host) {
					t.Errorf("%s: the canonical host survives:\n in  %s\n out %s", sh, in, out)
					continue
				}
				// What is served must name the variant's *whole* origin — a
				// variant on another port or another scheme is a different
				// server, and pointing the browser at the bare host sends it
				// somewhere nothing is listening.
				if !strings.Contains(out, variant.HostPort()) {
					t.Errorf("%s: the variant's host:port is not in the output:\n in  %s\n out %s\n want %s",
						sh, in, out, variant.HostPort())
					continue
				}
				// And the request direction has to be able to reverse it, or the
				// variant hostname is what reaches the database.
				back := string(HostLeaks(rev, []byte(out), true))
				if strings.Contains(back, variant.Host) && variant.Host != canon.Host {
					t.Errorf("%s: a variant hostname survives the request direction:\n out  %s\n back %s",
						sh, out, back)
				}
			}
		})
	}
}

// A map the engine cannot represent must be refused rather than silently
// half-honoured. The scan ignores the scheme when choosing a pair and the host
// table discards it, so one canonical host with two schemes and two variants
// resolved to whichever pair came first in one half and whichever was written
// last in the other — two blogs served from each other's worktree.
func TestOneHostCannotMapTwoWays(t *testing.T) {
	_, err := origin.NewMatcher([]origin.Pair{
		{Name: "one", Canonical: origin.MustParse("http://a.example.fi"), Variant: origin.MustParse("http://one.ddev.site")},
		{Name: "two", Canonical: origin.MustParse("https://a.example.fi"), Variant: origin.MustParse("https://two.ddev.site")},
	})
	if err == nil {
		// NewMatcher may build it; Validate is where the contract lives.
		m, _ := origin.NewMatcher([]origin.Pair{
			{Name: "one", Canonical: origin.MustParse("http://a.example.fi"), Variant: origin.MustParse("http://one.ddev.site")},
			{Name: "two", Canonical: origin.MustParse("https://a.example.fi"), Variant: origin.MustParse("https://two.ddev.site")},
		})
		if err = m.Validate(); err == nil {
			t.Fatal("a canonical host mapping two ways was accepted")
		}
	}
	if !strings.Contains(err.Error(), "maps to both") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}
