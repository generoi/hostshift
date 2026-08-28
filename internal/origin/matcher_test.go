package origin

import (
	"testing"
)

func mk(t *testing.T, from, to string) *Matcher {
	t.Helper()
	m, err := NewMatcher([]Pair{{Name: "main", Canonical: MustParse(from), Variant: MustParse(to)}})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func rw(t *testing.T, m *Matcher, in string) string {
	t.Helper()
	out, _ := m.Rewrite([]byte(in), "test", false)
	return string(out)
}

// TestAnchoredOriginsNotBareHosts is the regression test for the failure that
// motivated this design. v0.2's byte-level body rules produced
//
//	https://acmecorp.ddev.site:8081:8081/wp/wp-includes/…
//
// because they matched a bare host that had already been rewritten. Matching on
// anchored origins cannot do that.
func TestAnchoredOriginsNotBareHosts(t *testing.T) {
	m := mk(t, "https://acmecorp.ddev.site", "https://wt-a--acmecorp.ddev.site")

	cases := []struct{ in, want string }{
		// The variant contains the canonical host as a substring. Anchoring is
		// what makes that safe, and is what permits the leftmost-label scheme.
		{"https://wt-a--acmecorp.ddev.site/x", "https://wt-a--acmecorp.ddev.site/x"},
		{"//wt-a--acmecorp.ddev.site/x", "//wt-a--acmecorp.ddev.site/x"},
		// A longer host that merely starts with the canonical one.
		{"https://acmecorp.ddev.site.evil.example/x", "https://acmecorp.ddev.site.evil.example/x"},
		// A subdomain of the canonical host.
		{"https://a.acmecorp.ddev.site/x", "https://a.acmecorp.ddev.site/x"},
		// Bare hostname in prose is explicitly out of scope (test 28).
		{"visit acmecorp.ddev.site for details", "visit acmecorp.ddev.site for details"},
		// The real thing.
		{"https://acmecorp.ddev.site/x", "https://wt-a--acmecorp.ddev.site/x"},
	}
	for _, c := range cases {
		if got := rw(t, m, c.in); got != c.want {
			t.Errorf("Rewrite(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// TestIdempotent is test 7 at the matcher level: a second pass is a fixed point.
func TestIdempotent(t *testing.T) {
	for _, c := range []struct{ from, to, in string }{
		{"https://acmecorp.ddev.site", "https://wt-a--acmecorp.ddev.site", "https://acmecorp.ddev.site/a"},
		{"https://www.acmecorp.fi", "https://wt-a--acmecorp.ddev.site", "https://www.acmecorp.fi/a"},
		// The port-only variant that produced the double-port bug.
		{"https://acmecorp.ddev.site", "https://acmecorp.ddev.site:8081", "https://acmecorp.ddev.site/a"},
	} {
		m := mk(t, c.from, c.to)
		once := rw(t, m, c.in)
		twice := rw(t, m, once)
		if once != twice {
			t.Errorf("from=%s to=%s: not a fixed point: %q -> %q -> %q", c.from, c.to, c.in, once, twice)
		}
	}
}

// TestEncodedForms covers PLAN §4.4's encoded origin forms. M0 measured 46
// percent-encoded occurrences in acmecorp' production database, and the
// JSON-escaped form is what the REST API emits at render time.
func TestEncodedForms(t *testing.T) {
	m := mk(t, "https://www.acmecorp.fi", "https://v.example")
	cases := []struct{ in, want string }{
		{`https://www.acmecorp.fi/a`, `https://v.example/a`},
		{`http://www.acmecorp.fi/a`, `https://v.example/a`},
		{`//www.acmecorp.fi/a`, `//v.example/a`},
		{`https:\/\/www.acmecorp.fi\/a`, `https:\/\/v.example\/a`},
		{`\/\/www.acmecorp.fi\/a`, `\/\/v.example\/a`},
		{`redirect_to=https%3A%2F%2Fwww.acmecorp.fi%2Fwp-admin%2F`, `redirect_to=https%3A%2F%2Fv.example%2Fwp-admin%2F`},
		{`%2F%2Fwww.acmecorp.fi%2Fa`, `%2F%2Fv.example%2Fa`},
		// Case-insensitive on scheme and host (PLAN §5.5).
		{`HTTPS://WWW.ACMECORP.FI/a`, `https://v.example/a`},
		// Explicit default port compares equal to the bare host, and serialises
		// without it.
		{`https://www.acmecorp.fi:443/a`, `https://v.example/a`},
		// A non-default port is a *different* origin and must not match.
		{`https://www.acmecorp.fi:8080/a`, `https://www.acmecorp.fi:8080/a`},
	}
	for _, c := range cases {
		if got := rw(t, m, c.in); got != c.want {
			t.Errorf("Rewrite(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// TestProtocolRelativeIsAnchoredLeft: "//host" is an origin at the start of a
// value or after a separator, and a path segment inside a longer URL.
func TestProtocolRelativeIsAnchoredLeft(t *testing.T) {
	m := mk(t, "https://c.example", "https://v.example")
	cases := []struct{ in, want string }{
		{`//c.example/a`, `//v.example/a`},
		{`url(//c.example/a)`, `url(//v.example/a)`},
		{`//c.example/a 1x, //c.example/b 2x`, `//v.example/a 1x, //v.example/b 2x`},
		// A doubled slash inside a path is not an origin.
		{`https://other.example//c.example/a`, `https://other.example//c.example/a`},
		{`path//c.example/a`, `path//c.example/a`},
	}
	for _, c := range cases {
		if got := rw(t, m, c.in); got != c.want {
			t.Errorf("Rewrite(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// TestVariantSchemeWins covers PLAN §5.3's plain-HTTP-listener case: a
// host-valued map would produce an unreachable https origin.
func TestVariantSchemeWins(t *testing.T) {
	m := mk(t, "https://c.example", "http://127.0.0.1:8080")
	if got, want := rw(t, m, `https://c.example/a`), `http://127.0.0.1:8080/a`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Protocol-relative has no scheme to carry, so only the host:port moves.
	if got, want := rw(t, m, `//c.example/a`), `//127.0.0.1:8080/a`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestIdentityNeverSplices is why test 24 holds by construction: when a pair maps
// to itself the matcher returns the input slice untouched, whatever form the
// origin was written in.
func TestIdentityNeverSplices(t *testing.T) {
	m := mk(t, "https://c.example", "https://c.example")
	if !m.Identity() {
		t.Fatal("Identity() should be true when canonical == variant")
	}
	for _, in := range []string{
		`https://c.example/a`,
		`http://c.example/a`,
		`//c.example/a`,
		`https://c.example:443/a`,
		`HTTPS://C.EXAMPLE/a`,
		`https:\/\/c.example\/a`,
		`https%3A%2F%2Fc.example%2Fa`,
	} {
		in := []byte(in)
		out, _ := m.Rewrite(in, "test", false)
		if string(out) != string(in) {
			t.Errorf("identity map changed %q to %q", in, out)
		}
		if &out[0] != &in[0] {
			t.Errorf("identity map copied %q instead of returning the input slice", in)
		}
	}
}

// TestMultisiteCrossBlog is test 10b: blog 1 linking to blog 2's canonical must
// land on blog 2's *variant*, not blog 1's.
func TestMultisiteCrossBlog(t *testing.T) {
	m, err := NewMatcher([]Pair{
		{Name: "main", Canonical: MustParse("https://www.acmecorp.fi"), Variant: MustParse("https://wt-a--acmecorp.ddev.site")},
		{Name: "nat", Canonical: MustParse("https://www.acmecorpnat.fi"), Variant: MustParse("https://nat.wt-a--acmecorp.ddev.site")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	in := `<a href="https://www.acmecorpnat.fi/x">on <a href="https://www.acmecorp.fi/y">`
	want := `<a href="https://nat.wt-a--acmecorp.ddev.site/x">on <a href="https://wt-a--acmecorp.ddev.site/y">`
	if got := rw(t, m, in); got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
}

// TestValidateRejectsBadMaps is tests 10d, 17 and 29c: overlapping and
// non-injective maps are refused at startup rather than mis-mapping at runtime.
func TestValidateRejectsBadMaps(t *testing.T) {
	cases := []struct {
		name  string
		pairs []Pair
	}{
		{"canonical declared twice", []Pair{
			{Name: "a", Canonical: MustParse("https://c.example"), Variant: MustParse("https://v1.example")},
			{Name: "b", Canonical: MustParse("https://c.example"), Variant: MustParse("https://v2.example")},
		}},
		{"not injective", []Pair{
			{Name: "a", Canonical: MustParse("https://c1.example"), Variant: MustParse("https://v.example")},
			{Name: "b", Canonical: MustParse("https://c2.example"), Variant: MustParse("https://v.example")},
		}},
		{"variant is matched by a canonical", []Pair{
			// The suffix-derived scheme the previous revision used:
			// nat.acmecorp.ddev.site -> nat.wt-a.acmecorp.ddev.site would be
			// fine, but a variant that a canonical origin token *matches* is not.
			{Name: "a", Canonical: MustParse("https://c.example"), Variant: MustParse("https://c.example:8081")},
			{Name: "b", Canonical: MustParse("https://c.example:8081"), Variant: MustParse("https://v.example")},
		}},
	}
	for _, c := range cases {
		m, err := NewMatcher(c.pairs)
		if err != nil {
			continue // rejected at build time is also fine
		}
		if err := m.Validate(); err == nil {
			t.Errorf("%s: Validate() accepted a map it should refuse", c.name)
		}
	}
}

// TestValidateAcceptsContainment is the invariant §5.4 actually states:
// containment between a canonical host and a variant host is *permitted*,
// because anchoring makes it safe. A substring ban would forbid the whole
// leftmost-label prefix scheme.
func TestValidateAcceptsContainment(t *testing.T) {
	m, err := NewMatcher([]Pair{
		{Name: "main", Canonical: MustParse("https://acmecorp.ddev.site"), Variant: MustParse("https://wt-a--acmecorp.ddev.site")},
		{Name: "nat", Canonical: MustParse("https://nat.acmecorp.ddev.site"), Variant: MustParse("https://nat.wt-a--acmecorp.ddev.site")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("Validate() rejected a legitimate leftmost-label map: %v", err)
	}
}

// TestExplainReasons: --explain has to say why a candidate was not rewritten
// (PLAN §5.8), which is the difference between a five-minute diagnosis and an
// afternoon.
func TestExplainReasons(t *testing.T) {
	m := mk(t, "https://c.example", "https://v.example")
	cases := []struct{ in, reason string }{
		{"https://c.example.evil/a", ReasonNotAURL},
		{"https://c.example:8080/a", ReasonHostNotInMap},
		{"path//c.example/a", ReasonUnanchored},
	}
	for _, c := range cases {
		_, events := m.Rewrite([]byte(c.in), "test", true)
		if len(events) != 1 {
			t.Errorf("Rewrite(%q): %d events, want 1", c.in, len(events))
			continue
		}
		if events[0].Action != ActionSkipped || events[0].Reason != c.reason {
			t.Errorf("Rewrite(%q): action=%q reason=%q, want skipped/%s", c.in, events[0].Action, events[0].Reason, c.reason)
		}
	}

	mi := mk(t, "https://c.example", "https://c.example")
	if _, ev := mi.Rewrite([]byte("https://c.example/a"), "test", true); len(ev) != 1 || ev[0].Reason != ReasonIdentityMap {
		t.Errorf("identity map should explain itself, got %+v", ev)
	}
}

// TestOriginNormalisation covers PLAN §5.5's comparison rules.
func TestOriginNormalisation(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://Example.COM", "https://example.com"},
		{"https://example.com:443", "https://example.com"},
		{"http://example.com:80", "http://example.com"},
		{"https://example.com:8080", "https://example.com:8080"},
		{"https://example.com.", "https://example.com"}, // trailing root dot
		{"https://example.com/a/b?c#d", "https://example.com"},
	}
	for _, c := range cases {
		o, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if o.String() != c.want {
			t.Errorf("Parse(%q) = %q, want %q", c.in, o.String(), c.want)
		}
	}
	for _, bad := range []string{"//example.com", "example.com", "ftp://example.com", "https://"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should have failed", bad)
		}
	}
}

// TestThirdPartyHostUntouched is test 8.
func TestThirdPartyHostUntouched(t *testing.T) {
	m := mk(t, "https://c.example", "https://v.example")
	in := `<img src="https://cdn.jsdelivr.net/x.js"><a href="https://wordpress.org/">`
	if got := rw(t, m, in); got != in {
		t.Errorf("third-party hosts were touched:\n got %q\nwant %q", got, in)
	}
}

// TestDelimiterIsNotAnAllowlist covers the audit's finding that an enumerated
// terminator set has the same long tail an attribute allowlist does. The CSP
// case was live: a source list names the origin without a trailing slash, so
// `default-src 'self' https://c.example; script-src …` went out naming only the
// canonical host and the browser blocked every resource on the variant.
func TestDelimiterIsNotAnAllowlist(t *testing.T) {
	m := mk(t, "https://c.example", "https://v.example")
	for _, c := range []struct{ in, want string }{
		{"default-src 'self' https://c.example; script-src 'self'", "default-src 'self' https://v.example; script-src 'self'"},
		{"@import url(https://c.example);", "@import url(https://v.example);"},
		{"[https://c.example]", "[https://v.example]"},
		{"a=https://c.example|b", "a=https://v.example|b"},
		{"{\"u\":https://c.example}", "{\"u\":https://v.example}"},
		// Still not a URL: the host simply continues.
		{"https://c.example.evil/x", "https://c.example.evil/x"},
	} {
		if got := rw(t, m, c.in); got != c.want {
			t.Errorf("Rewrite(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// TestPercentEncodedDotIsPartOfTheHost: '%' is not a host byte, but "%2E" is a
// dot, so treating '%' as an unconditional terminator rewrote a *different*
// registrable domain — one the code correctly refuses when the dot is literal.
func TestPercentEncodedDotIsPartOfTheHost(t *testing.T) {
	m := mk(t, "https://c.example", "https://v.example")
	for _, in := range []string{
		"https://c.example%2Eattacker.test/p", // %2E is '.'
		"https://c.example%2Dx.test/p",        // %2D is '-'
	} {
		if got := rw(t, m, in); got != in {
			t.Errorf("Rewrite(%q) = %q — that is a different host", in, got)
		}
	}
	// A percent-escape that really does terminate the host still works.
	if got, want := rw(t, m, "https%3A%2F%2Fc.example%2Fwp-admin"), "https%3A%2F%2Fv.example%2Fwp-admin"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestPercentEncodedPort is the port family of the double-port bug. "%3A" is how
// a port separator appears inside redirect_to=, and reading only ':' made it a
// terminator — so an origin on a port nothing listens on was rewritten to the
// variant, and a canonical origin that *did* carry the mapped port was missed.
func TestPercentEncodedPort(t *testing.T) {
	plain := mk(t, "https://c.example", "https://v.example")
	in := "https%3A%2F%2Fc.example%3A8443%2Fx"
	if got := rw(t, plain, in); got != in {
		t.Errorf("a different origin was rewritten: %q -> %q", in, got)
	}

	ported := mk(t, "https://c.example:8443", "https://v.example")
	want := "https%3A%2F%2Fv.example%2Fx"
	if got := rw(t, ported, in); got != want {
		t.Errorf("the mapped port was missed:\n got %q\nwant %q", got, want)
	}
}

// TestIDNMatchesTheUnicodeLabel: WordPress does not punycode siteurl, so a site
// declared with a Unicode host has the U-label in its database and in every
// rendered page. Building patterns from the A-label alone matched nothing at
// all, and the whole page leaked.
func TestIDNMatchesTheUnicodeLabel(t *testing.T) {
	m := mk(t, "https://hämeen.fi", "https://v.example")
	for _, c := range []struct{ in, want string }{
		{"https://hämeen.fi/x", "https://v.example/x"},
		{"https://HÄMEEN.FI/x", "https://v.example/x"},
		{"https://xn--hmeen-gra.fi/x", "https://v.example/x"},
	} {
		if got := rw(t, m, c.in); got != c.want {
			t.Errorf("Rewrite(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}
