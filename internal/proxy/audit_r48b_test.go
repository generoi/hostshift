package proxy

import (
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// TestR48ACookieDomainIsMatchedInBothSpellingsOfAnIDN.
//
// `isCanonicalDomain` compared raw bytes: `c.Host` is always punycode, and the
// `Domain=` attribute is whatever the application wrote. So a site declared
// `https://hämeenlinna.fi` matched a cookie for `.xn--hmeenlinna-q5a.fi` and not
// one for `.hämeenlinna.fi` — the spelling the developer typed, and the one
// WordPress derives itself when COOKIE_DOMAIN is unset, which is how most of
// this fleet's Bedrock configs are written.
//
// A cookie whose Domain the browser is not on is discarded, and this one is the
// login cookie. It is the only host comparison in the codebase that was not
// folded through NormaliseHost.
func TestR48ACookieDomainIsMatchedInBothSpellingsOfAnIDN(t *testing.T) {
	m, err := origin.NewMap([]origin.Site{{
		Name:      "hml",
		Canonical: origin.MustParse("https://hämeenlinna.fi"),
		Variant:   origin.MustParse("https://wt-a--hml.ddev.site"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{Map: m}

	for _, d := range []string{
		"hämeenlinna.fi", ".hämeenlinna.fi",
		"xn--hmeenlinna-q5a.fi", ".xn--hmeenlinna-q5a.fi",
		"HÄMEENLINNA.FI",
	} {
		if !p.isCanonicalDomain(strings.TrimPrefix(d, ".")) {
			t.Errorf("Domain=%s is this site, in one of its spellings, and was not matched", d)
		}
	}
	// And a domain that is not the site stays unmatched.
	for _, d := range []string{"example.fi", "hameenlinna.fi", "notxn--hmeenlinna-q5a.fi"} {
		if p.isCanonicalDomain(d) {
			t.Errorf("Domain=%s is a different host and was matched", d)
		}
	}
}
