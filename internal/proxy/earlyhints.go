package proxy

import (
	"net/http"
	"net/http/httptrace"
	"net/textproto"
)

// hintRewriter runs the Tier 1 header list over an informational (1xx) response
// before httputil forwards it to the browser.
//
// `httputil.ReverseProxy` handles 1xx itself, through an
// `httptrace.Got1xxResponse` hook that copies the header verbatim and calls
// WriteHeader. `ModifyResponse` is never consulted, so `originHeaders` — which
// PLAN §5.2 calls "the whole guarantee for the header surface" — never ran on a
// `103 Early Hints`. A browser *preloads* from a 103, so
// `Link: <https://canonical/style.css>; rel=preload` is a fetch issued to
// production before the page arrives, while the identical header on the final
// 200 came out as the variant.
//
// Why this is a RoundTripper and not a hook on the inbound request, which is
// what was tried first and reverted: `httptrace.WithClientTrace` **composes**
// rather than replaces, and of two composed hooks the one added *last* runs
// *first*. A trace attached in the handler is on the context before
// `ReverseProxy.ServeHTTP` adds its own, so httputil's ran first, wrote the
// header, and the rewrite landed on bytes already on the wire.
//
// Inside the transport that order inverts for free. httputil has already
// attached its trace to the outbound request by the time RoundTrip is called,
// so the one added here is the later one and runs first — mutating the very
// header map httputil then copies. No fork of ReverseProxy, no second
// WriteHeader, nothing racing anything.
//
// The mutation is in place because that map *is* what httputil reads.
// Returning a new header would rewrite a copy nobody forwards.
type hintRewriter struct {
	base http.RoundTripper
	p    *Proxy
}

func (h *hintRewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		Got1xxResponse: func(code int, hdr textproto.MIMEHeader) error {
			// Location is skipped on a 1xx for the same reason the final
			// response's guard exists: a self-redirect carve-out needs the
			// request URL to compare against, and a 1xx carries no redirect
			// anyway — 103 is the only informational response that carries
			// origins, and it carries them in Link.
			h.p.rewriteOriginHeaders(http.Header(hdr), true)
			return nil
		},
	})
	return h.base.RoundTrip(req.WithContext(ctx))
}

// transport is the RoundTripper the proxy runs, wrapping whatever it was given.
func (p *Proxy) transport() http.RoundTripper {
	base := p.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	return &hintRewriter{base: base, p: p}
}
