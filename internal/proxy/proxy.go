package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// Proxy is the hostshift reverse proxy.
//
// M1 is the shell: one upstream, one canonical host, the response HTML surface,
// and the ReverseProxy mechanics from PLAN §5.7 — every one of which is a silent
// failure if missed. M2 adds the host map, the multisite inverse mapping,
// request bodies and the self-redirect guard.
type Proxy struct {
	Upstream *url.URL
	Matcher  *origin.Matcher
	Stats    *rewrite.Stats
	DryRun   bool
	Log      *slog.Logger

	// Canonical is the Host presented upstream. M2 replaces this single value
	// with the per-blog inverse of the host map (PLAN §5.1).
	Canonical string
}

// originHeaders are the Tier 1 response headers whose values carry origins
// (PLAN §5.2). Set-Cookie is deliberately absent: its Domain= attribute wants
// dropping rather than substituting, which is M2 and test 2.
var originHeaders = []string{
	"Location",
	"Content-Location",
	"Refresh",
	"Link",
	"Content-Security-Policy",
	"Content-Security-Policy-Report-Only",
	"Access-Control-Allow-Origin",
}

// rewritableHTML reports whether a Content-Type enters the HTML rewriter.
//
// The gate is Content-Type, not a body scan (PLAN §5.2). Learning that a 4 MB
// JPEG contains no canonical host would mean reading 4 MB through the automaton
// — buffering exactly the bodies most worth streaming — when the header answered
// it for free. text/css and application/javascript are deliberately excluded per
// Tier 2: 88 CSS and 185 JS files in the fleet's themes, zero absolute URLs.
func rewritableHTML(ct string) bool {
	mt := ct
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	return strings.EqualFold(strings.TrimSpace(mt), "text/html")
}

// Handler builds the http.Handler.
func (p *Proxy) Handler() http.Handler {
	log := p.Log
	if log == nil {
		log = slog.Default()
	}
	rp := &httputil.ReverseProxy{
		// Rewrite, not Director — setting both is an error.
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(p.Upstream)

			// SetURL ends with r.Out.Host = "". Assign the canonical Host
			// *after* it, or the upstream sees the container's host and
			// multisite resolution fails silently.
			if p.Canonical != "" {
				r.Out.Host = p.Canonical
			}

			// SetXForwarded sets X-Forwarded-Proto: http whenever r.In.TLS is
			// nil, which is always — hostshift listens plain. Setting https
			// after it, never before, is what stops wp-login.php redirecting
			// forever: load.php makes is_ssl() true only via $_SERVER['HTTPS']
			// or SERVER_PORT 443, and application.php reads
			// HTTP_X_FORWARDED_PROTO.
			r.SetXForwarded()
			r.Out.Header.Set("X-Forwarded-Proto", "https")

			// Never send X-Forwarded-Port. It is not hop-by-hop, so it would be
			// forwarded verbatim; making production trust it is the one lesson
			// PLAN §2.3 draws without qualification.
			r.Out.Header.Del("X-Forwarded-Port")

			// Identity upstream: the fleet has no compression config of its own,
			// so DDEV's stock nginx applies, and asking for identity means the
			// common path needs no decoder at all. Setting it explicitly also
			// stops Go's transport adding its own transparent gzip.
			r.Out.Header.Set("Accept-Encoding", "identity")
		},
		ModifyResponse: p.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Test 14: upstream failure is surfaced, not swallowed.
			log.Error("upstream request failed", "url", r.URL.String(), "err", err)
			http.Error(w, "hostshift: upstream request failed: "+err.Error(), http.StatusBadGateway)
		},
	}
	return rp
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	explain := p.Stats.Explain()
	changed := false

	for _, h := range originHeaders {
		vs := resp.Header.Values(h)
		for i, v := range vs {
			out, events := p.Matcher.Rewrite([]byte(v), rewrite.SurfaceHeader, explain)
			p.Stats.Record(rewrite.SurfaceHeader, 0, events)
			if !p.DryRun && string(out) != v {
				vs[i] = string(out)
				changed = true
			}
		}
	}

	if !rewritableHTML(resp.Header.Get("Content-Type")) {
		return nil
	}
	resp.Body = rewrite.NewHTML(resp.Body, p.Matcher, resp.Body, rewrite.Options{
		DryRun: p.DryRun,
		Stats:  p.Stats,
	})
	changed = true

	if changed && !p.DryRun {
		// Every rewrite changes body length, so no stale length may be
		// forwarded. ContentLength is cleared alongside the header because
		// flushInterval keys off the struct field, not the header; leaving it
		// stale disables periodic flushing.
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1

		// Drop the validators. Otherwise a conditional request returns 304 and
		// the browser serves a cached canonical-bearing body, defeating test 28
		// silently.
		resp.Header.Del("ETag")
		resp.Header.Del("Last-Modified")

		// A rewritten body is not byte-range addressable.
		resp.Header.Del("Accept-Ranges")
	}
	return nil
}
