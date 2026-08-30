// Package origin holds the origin model and the anchored origin matcher that
// every rewrite in hostshift goes through.
//
// PLAN §5.3: the map is origin→origin (scheme + host + port), never host→host.
// PLAN §5.5: net/url is used for *comparison only*. Nothing is ever
// re-serialised through URL.String(), which lowercases the scheme and
// percent-encodes the path — that alone would break test 24.
package origin

import (
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
)

// Origin is a normalised scheme + host + port.
//
// Normalisation, per PLAN §5.5:
//   - scheme and host are lowercased
//   - the host is punycode ("real for .fi client domains")
//   - a trailing root dot is stripped
//   - a port equal to the scheme's default is stored as ""
//
// The zero Origin is invalid; use Parse.
type Origin struct {
	Scheme string // "http" or "https"
	Host   string // lowercase, punycode, no trailing dot
	Port   string // "" when it is the scheme's default port
}

// Parse normalises a declared origin. A trailing "/" is accepted and ignored —
// people write origins that way — but any longer path is an error rather than
// something quietly discarded. hostshift maps origins, not URL prefixes, so
// `--map https://a.example/blog=https://b.example` cannot do what its author
// meant, and silently dropping the path let them believe it had.
//
// A protocol-relative "//host" is rejected here — a *declared* origin must name
// its scheme (PLAN §5.3, the plain-HTTP-listener case). The matcher handles
// protocol-relative occurrences in content separately, where scheme is genuinely
// unknown.
func Parse(s string) (Origin, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return Origin{}, fmt.Errorf("parse origin %q: %w", s, err)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http", "https":
	case "":
		return Origin{}, fmt.Errorf("parse origin %q: scheme is required (http:// or https://)", s)
	default:
		return Origin{}, fmt.Errorf("parse origin %q: unsupported scheme %q", s, u.Scheme)
	}
	if u.Hostname() == "" {
		return Origin{}, fmt.Errorf("parse origin %q: no host", s)
	}
	if p := strings.TrimSuffix(u.EscapedPath(), "/"); p != "" {
		return Origin{}, fmt.Errorf(
			"parse origin %q: an origin is a scheme and a host, with no path — drop %q", s, p)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return Origin{}, fmt.Errorf("parse origin %q: an origin is a scheme and a host, nothing else", s)
	}
	host, err := normaliseHost(u.Hostname())
	if err != nil {
		return Origin{}, fmt.Errorf("parse origin %q: %w", s, err)
	}
	return Origin{Scheme: scheme, Host: host, Port: NormalisePort(scheme, u.Port())}, nil
}

// MustParse is Parse for tests and literals.
func MustParse(s string) Origin {
	o, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return o
}

// hostFold is the browser's domain-to-ASCII: UTS46 mapping, then punycode.
//
// idna.Punycode punycodes and nothing else — no UTS46 mapping, no NFC, no
// removal of ignorable code points — while the WHATWG parser runs domain-to-ASCII
// with beStrict=false, which maps or deletes a large class of code points first.
// Every spelling UTS46 folds onto a canonical host was therefore invisible: a
// soft hyphen inside the host, fullwidth letters, U+3002/U+FF0E/U+FF61 as label
// separators, a zero-width space, and — the one that turns up without an
// attacker — NFD, which macOS filesystems and pasted content produce. All six
// resolve to the canonical origin in a browser.
//
// idna.Lookup is not the answer either: it rejects hosts that are invalid for
// resolution, and this is a comparison form, so it must fold rather than judge.
// MapForLookup with StrictDomainName off maps without rejecting, and
// Transitional(false) is what browsers do.
//
// CheckHyphens(false) because WHATWG's domain-to-ASCII sets it false explicitly,
// and MapForLookup turns it on. With it on, x/net *errors* where the browser
// succeeds — on any label with `--` in positions 3-4, or a leading or trailing
// hyphen — and the fallback then compares the raw string, which shares no bytes
// with anything. One such host in the map silently switched the whole fold off
// for that host on every surface. `--` at 3-4 is not exotic: it is the shape the
// add-on's own default slug produces, `wt--acme.ddev.site`, and a variant is the
// canonical side of the request-direction matcher.
var hostFold = idna.New(
	idna.MapForLookup(),
	idna.StrictDomainName(false),
	idna.CheckHyphens(false),
	idna.Transitional(false),
)

func normaliseHost(h string) (string, error) {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	a, err := hostFold.ToASCII(h)
	if err != nil {
		// A host this cannot map is still a host someone declared. Fall back to
		// the punycode-only form rather than refusing the configuration: being
		// unable to fold an exotic spelling is a reason to compare it literally,
		// not a reason to fail at startup.
		if p, perr := idna.Punycode.ToASCII(h); perr == nil {
			return p, nil
		}
		return "", fmt.Errorf("punycode %q: %w", h, err)
	}
	return a, nil
}

// HostFold exposes the browser's domain-to-ASCII for callers that parse a host
// out of content rather than out of a configuration.
func HostFold(h string) (string, error) { return hostFold.ToASCII(h) }

// NormalisePort returns "" for a scheme's default port, so that
// https://h and https://h:443 compare equal (PLAN §5.5).
func NormalisePort(scheme, port string) string {
	switch {
	case port == "":
		return ""
	case scheme == "https" && port == "443":
		return ""
	case scheme == "http" && port == "80":
		return ""
	}
	return port
}

// HostPort renders host[:port], omitting a default port.
func (o Origin) HostPort() string {
	if o.Port == "" {
		return o.Host
	}
	return o.Host + ":" + o.Port
}

// String renders scheme://host[:port].
//
// This is for building a *replacement*, and for diagnostics. It is never used to
// round-trip input — see the package comment.
func (o Origin) String() string { return o.Scheme + "://" + o.HostPort() }

// Equal reports whether two normalised origins are the same origin.
func (o Origin) Equal(b Origin) bool {
	return o.Scheme == b.Scheme && o.Host == b.Host && o.Port == b.Port
}
