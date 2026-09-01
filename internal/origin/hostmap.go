package origin

import (
	"fmt"
	"net"
	"strings"
)

// Site is one blog: a set of canonical origins that all map to a single variant.
//
// Canonical is the primary origin — the production hostname under
// production-canonical — and is what the *request* direction maps back to.
// Aliases are the same blog's other environments (its ddev host, staging,
// whatever else appears in content). Listing them is what lets a residual
// @production or @staging URL left behind by an imperfect db:pull be corrected
// too (PLAN §4.2), and it is why the forward map is many→one.
type Site struct {
	Name      string
	Canonical Origin
	Aliases   []Origin
	Variant   Origin
}

// CanonicalSet is every origin that rewrites to this site's variant.
func (s Site) CanonicalSet() []Origin {
	out := make([]Origin, 0, 1+len(s.Aliases))
	return append(append(out, s.Canonical), s.Aliases...)
}

// Map is the resolved host map, with a matcher for each direction.
//
// Multisite is N→N with unrelated registrable domains (PLAN §4.2), so both
// directions have to be per-blog: a request for nat.V must arrive upstream as
// the *sibling blog's* canonical host, not the network's main host, because
// get_site_by_path matches wp_blogs.domain exactly.
type Map struct {
	Sites []Site

	forward   *Matcher // canonical set -> variant, for responses
	reverse   *Matcher // variant -> canonical, for requests
	byVariant map[string]int
}

// NewMap validates and resolves a set of sites. Every error it returns is a
// configuration error and must stop startup (PLAN §5.3, §5.4).
func NewMap(sites []Site) (*Map, error) {
	if len(sites) == 0 {
		return nil, fmt.Errorf("origin: no sites in the map")
	}

	m := &Map{Sites: sites, byVariant: map[string]int{}}

	owner := map[Origin]string{}
	var fwd, rev []Pair
	for i, s := range sites {
		if s.Name == "" {
			return nil, fmt.Errorf("origin: site %d has no name", i+1)
		}
		if prev, dup := m.byVariant[s.Variant.Host]; dup {
			return nil, fmt.Errorf("origin: sites %q and %q derive the same variant host %s",
				sites[prev].Name, s.Name, s.Variant.Host)
		}
		m.byVariant[s.Variant.Host] = i

		for _, c := range s.CanonicalSet() {
			if prev, dup := owner[c]; dup {
				return nil, fmt.Errorf("origin: canonical %s is declared by both %q and %q", c, prev, s.Name)
			}
			owner[c] = s.Name
			fwd = append(fwd, Pair{Name: s.Name, Canonical: c, Variant: s.Variant})
		}
		rev = append(rev, Pair{Name: s.Name, Canonical: s.Variant, Variant: s.Canonical})
	}

	// A generated variant must not collide with *another site's* canonical
	// origin — that would route two blogs to one host. Exact-origin equality,
	// not suffix or substring: containment is permitted and is what the
	// leftmost-label scheme relies on (PLAN §5.4).
	//
	// Colliding with the site's *own* canonical set is not an error: that is
	// exactly the identity map, which test 24 requires be a legal, no-op
	// configuration.
	for _, s := range sites {
		if prev, clash := owner[s.Variant]; clash && prev != s.Name {
			return nil, fmt.Errorf("origin: variant %s for site %q collides with a canonical origin of %q",
				s.Variant, s.Name, prev)
		}
	}

	var err error
	if m.forward, err = NewMatcher(fwd); err != nil {
		return nil, err
	}
	if err := m.forward.Validate(); err != nil {
		return nil, err
	}
	if m.reverse, err = NewMatcher(rev); err != nil {
		return nil, err
	}
	if err := m.reverse.Validate(); err != nil {
		return nil, fmt.Errorf("request direction: %w", err)
	}
	return m, nil
}

// Forward maps canonical origins to variants, for responses.
func (m *Map) Forward() *Matcher { return m.forward }

// Reverse maps variants back to canonical origins, for requests.
func (m *Map) Reverse() *Matcher { return m.reverse }

// Identity reports whether nothing can be rewritten in either direction.
func (m *Map) Identity() bool { return m.forward.Identity() }

// SiteForHost resolves a request's Host header to a site.
//
// Routing is on the host alone; a port is accepted and ignored. Behind DDEV's
// router the Host arrives without one, and refusing a request because the
// browser happened to include :443 would be a 421 for no reason. The *map* is
// still origin-keyed — this is only which blog the request belongs to.
func (m *Map) SiteForHost(hostHeader string) (*Site, bool) {
	h := hostHeader
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i+1:], "]") {
		// Strip a port, but not the tail of a bracketed IPv6 literal.
		if p := h[i+1:]; p == "" || isAllDigits(p) {
			h = h[:i]
		}
	}
	h = strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
	if norm, err := normaliseHost(h); err == nil {
		h = norm
	}
	if i, ok := m.byVariant[h]; ok {
		return &m.Sites[i], true
	}
	return nil, false
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return len(s) > 0
}

// String renders the map for `hostshift map`.
func (m *Map) String() string {
	nameW, canonW := 4, 9
	for _, s := range m.Sites {
		nameW = max(nameW, len(s.Name))
		for _, o := range s.CanonicalSet() {
			canonW = max(canonW, len(o.String()))
		}
	}
	var sb strings.Builder
	for _, s := range m.Sites {
		fmt.Fprintf(&sb, "%-*s  %-*s  ->  %s\n", nameW, s.Name, canonW, s.Canonical, s.Variant)
		for _, a := range s.Aliases {
			fmt.Fprintf(&sb, "%-*s  %-*s      (alias)\n", nameW, "", canonW, a)
		}
	}
	return sb.String()
}

// ResolvesLocally reports whether a hostname can never reach another machine:
// the loopback names and addresses, and the reserved TLDs that are never
// delegated. `.test` is RFC 6761 and has no root delegation at all.
//
// It exists because two places were answering this question differently. `diff`
// used its own copy to decide whether a crawl would hit the client's live site,
// while the map diagnostics used a TLD suffix test alone — so `additional_fqdns:
// [acme.test]` was reported as canonical-on-production by one half of the same
// binary that the other half already knew could not resolve anywhere.
func ResolvesLocally(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return strings.HasSuffix(h, ".test")
}
