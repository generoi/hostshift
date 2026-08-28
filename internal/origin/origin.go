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

// Parse normalises a declared origin. Input may carry a path, which is ignored:
// callers declare origins, not URLs.
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

func normaliseHost(h string) (string, error) {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	// idna.Lookup rejects hosts that are invalid for resolution; ToASCII on the
	// Punycode profile is the comparison form we want and leaves ASCII untouched.
	a, err := idna.Punycode.ToASCII(h)
	if err != nil {
		return "", fmt.Errorf("punycode %q: %w", h, err)
	}
	return a, nil
}

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
