package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// DefaultMaxBody is the request-body buffering cap (PLAN §5.8). Above it a body
// streams through untouched and the skip is logged.
const DefaultMaxBody = 8 << 20

// Proxy is the hostshift reverse proxy.
//
// The transformation is bidirectional. Responses map canonical origins to the
// variant so the browser stays on the variant host; requests map the variant
// back to canonical so WordPress sees exactly the host its database was written
// for. Response-only rewriting is insufficient — Gutenberg would receive variant
// URLs and save them straight back into the database (PLAN §4.4, §5.1).
type Proxy struct {
	Upstream *url.URL
	Map      *origin.Map
	Stats    *rewrite.Stats
	DryRun   bool

	// StrictOrigins turns off the self-redirect carve-out, returning 404 where
	// the guard would otherwise let a canonical Location through (PLAN §4.4,
	// test 32). Used by the corpus diff and test 28's full crawl.
	StrictOrigins bool

	// MaxBody caps request-body buffering. Zero means DefaultMaxBody.
	MaxBody int64

	Log *slog.Logger
}

// originHeaders are the Tier 1 response headers whose values carry origins
// (PLAN §5.2). Set-Cookie is handled separately: its Domain= attribute wants
// dropping rather than substituting.
var originHeaders = []string{
	"Location",
	"Content-Location",
	"Refresh",
	"Link",
	"Content-Security-Policy",
	"Content-Security-Policy-Report-Only",
	"Access-Control-Allow-Origin",
}

// requestOriginHeaders carry origins on the way in and must be mapped back to
// canonical.
//
// Referer is load-bearing: functions.php runs it through
// wp_validate_redirect($ref, false), which rejects any host that is not
// home_url()'s, so without this wp_get_referer() is false throughout wp-admin
// and bulk actions and back-links degrade.
var requestOriginHeaders = []string{"Referer", "Origin"}

type ctxKey int

const stateKey ctxKey = 0

// state carries what the response direction needs to know about the request.
type state struct {
	site *origin.Site
	// url is the absolute URL the browser asked for, in variant space. The
	// self-redirect guard compares against it.
	url  string
	body []byte
}

func (p *Proxy) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

func (p *Proxy) maxBody() int64 {
	if p.MaxBody > 0 {
		return p.MaxBody
	}
	return DefaultMaxBody
}

// Handler builds the http.Handler.
func (p *Proxy) Handler() http.Handler {
	rp := &httputil.ReverseProxy{
		Rewrite:        p.rewriteRequest,
		ModifyResponse: p.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Test 14: upstream failure is surfaced, not swallowed.
			p.log().Error("upstream request failed", "url", r.URL.String(), "err", err)
			http.Error(w, "hostshift: upstream request failed: "+err.Error(), http.StatusBadGateway)
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site, ok := p.Map.SiteForHost(r.Host)
		if !ok {
			// Test 16. 421 is the honest answer: this server is not authoritative
			// for that host. Proxying it anyway would send an unmapped Host
			// upstream and resolve to the wrong blog, silently.
			p.log().Warn("request host is not in the map", "host", r.Host)
			http.Error(w, "hostshift: no site is mapped to host "+r.Host+
				"\nrun `hostshift map` to see the resolved map", http.StatusMisdirectedRequest)
			return
		}

		st := &state{site: site, url: "https://" + r.Host + r.URL.RequestURI()}
		p.rewriteRequestBody(r, st)
		rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), stateKey, st)))
	})
}

func (p *Proxy) rewriteRequest(r *httputil.ProxyRequest) {
	st, _ := r.In.Context().Value(stateKey).(*state)
	rev := p.Map.Reverse()
	explain := p.Stats.Explain()

	r.SetURL(p.Upstream)

	// SetURL ends with r.Out.Host = "". Assigning the canonical Host *after* it
	// is what makes multisite work: ms-settings.php lowercases and strips
	// :80/:443, then get_site_by_path matches wp_blogs.domain exactly. The
	// sibling blog's own canonical host is required here, not the network's
	// main host.
	if st != nil {
		r.Out.Host = st.site.Canonical.HostPort()
	}

	// SetXForwarded writes X-Forwarded-Proto: http whenever r.In.TLS is nil,
	// which is always — hostshift listens plain. Setting https *after* it is
	// what stops wp-login.php redirecting forever: load.php makes is_ssl() true
	// only via $_SERVER['HTTPS'] or SERVER_PORT 443, and application.php reads
	// HTTP_X_FORWARDED_PROTO.
	r.SetXForwarded()
	r.Out.Header.Set("X-Forwarded-Proto", "https")

	// Never send X-Forwarded-Port. It is not hop-by-hop, so it would be
	// forwarded verbatim; making production trust it is the one lesson PLAN §2.3
	// draws without qualification.
	r.Out.Header.Del("X-Forwarded-Port")

	// X-Forwarded-Host goes for the same reason, and it is the more likely of
	// the two to do damage: SetXForwarded fills it with the *variant* host, so
	// anything in Bedrock or a plugin that prefers it over Host puts the variant
	// straight back inside WordPress — undoing the request mapping this function
	// exists to perform.
	r.Out.Header.Del("X-Forwarded-Host")

	// Identity upstream: the fleet has no compression config of its own, so
	// DDEV's stock nginx applies, and asking for identity means the common path
	// needs no decoder. Setting it explicitly also stops Go's transport adding
	// its own transparent gzip.
	r.Out.Header.Set("Accept-Encoding", "identity")

	// The query string, byte for byte. This is not optional:
	// wp-login.php?redirect_to=… is validated by wp_validate_redirect() against
	// home_url()'s host, so a variant origin is silently discarded and login
	// returns to the wrong place. The percent-encoded form is in the token set,
	// which is what makes redirect_to=https%3A%2F%2F… work.
	if q := r.Out.URL.RawQuery; q != "" {
		out, ev := rev.Rewrite([]byte(q), rewrite.SurfaceRequestLine, explain)
		p.Stats.Record(rewrite.SurfaceRequestLine, 0, ev)
		if !p.DryRun {
			r.Out.URL.RawQuery = string(out)
		}
	}

	// The path, for the routes that carry an origin in it. Both fields are set
	// so that EscapedPath() returns exactly these bytes rather than re-encoding
	// — URL.String() percent-encodes, which would break test 24's spirit.
	if esc := r.Out.URL.EscapedPath(); esc != "" {
		out, ev := rev.Rewrite([]byte(esc), rewrite.SurfaceRequestLine, explain)
		p.Stats.Record(rewrite.SurfaceRequestLine, 0, ev)
		if !p.DryRun && string(out) != esc {
			if dec, err := url.PathUnescape(string(out)); err == nil {
				r.Out.URL.Path = dec
				r.Out.URL.RawPath = string(out)
			}
		}
	}

	for _, h := range requestOriginHeaders {
		vs := r.Out.Header.Values(h)
		for i, v := range vs {
			out, ev := rev.Rewrite([]byte(v), rewrite.SurfaceHeader, explain)
			p.Stats.Record(rewrite.SurfaceHeader, 0, ev)
			if !p.DryRun {
				vs[i] = string(out)
			}
		}
	}

	// http.Request.ContentLength drives the transport, not the header. After
	// rewriting a body it must be reset, TransferEncoding cleared, and GetBody
	// provided, or the transport errors or truncates.
	if st != nil && st.body != nil {
		body := st.body
		r.Out.Body = io.NopCloser(bytes.NewReader(body))
		r.Out.ContentLength = int64(len(body))
		r.Out.TransferEncoding = nil
		r.Out.Header.Set("Content-Length", itoa(len(body)))
		r.Out.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}
}

func (p *Proxy) modifyResponse(resp *http.Response) error {
	st, _ := resp.Request.Context().Value(stateKey).(*state)
	fwd := p.Map.Forward()
	explain := p.Stats.Explain()
	changed := false

	// The self-redirect guard (PLAN §4.4, test 32). 55 of the fleet's 63 DDEV
	// repos ship an nginx snippet that 302s a missing /app/uploads/ request to a
	// hardcoded remote origin. Rewriting that Location sends the browser back to
	// the request it just made: an infinite redirect loop. Passing it through
	// unmodified is the one enumerated carve-out to test 28.
	//
	// Two things this deliberately does *not* do, both of which it used to.
	//
	// It does not fire on an unsafe method. The carve-out's whole justification
	// is that it is "a read-only asset GET, not the write hazard §4.4 opens
	// with" — but there was no method test, so a POST to admin-post.php answered
	// with a self-redirect sent the browser to live production on a write path,
	// which is that exact hazard.
	//
	// It does not skip the rest of the response. It used to return early, which
	// jumped over every other Tier 1 header and the Set-Cookie Domain= drop, so
	// Link and CSP went out naming the canonical host and a Domain=.canonical
	// cookie survived. Test 28 enumerates the carve-out as *one* header; only
	// Location is exempt, and only for the duration of this block.
	skipLocation := false
	if st != nil && isRedirect(resp.StatusCode) && safeMethod(resp.Request) {
		if loc := resp.Header.Get("Location"); loc != "" {
			rewritten, _ := fwd.Rewrite([]byte(loc), rewrite.SurfaceHeader, false)
			if sameURL(string(rewritten), st.url) {
				if p.StrictOrigins {
					p.log().Warn("self-redirect suppressed by --strict-origins", "url", st.url)
					blank(resp, http.StatusNotFound,
						"hostshift: "+st.url+" does not exist locally, and --strict-origins forbids "+
							"redirecting the browser to the canonical origin\n")
					return nil
				}
				p.Stats.Record(rewrite.SurfaceHeader, 0, []origin.Event{{
					Surface: rewrite.SurfaceHeader, Text: loc,
					Action: origin.ActionSkipped, Reason: origin.ReasonSelfRedirect,
				}})
				p.log().Info("self-redirect passed through", "location", loc)
				skipLocation = true
			}
		}
	}

	for _, h := range originHeaders {
		if skipLocation && h == "Location" {
			continue
		}
		vs := resp.Header.Values(h)
		for i, v := range vs {
			out, ev := fwd.Rewrite([]byte(v), rewrite.SurfaceHeader, explain)
			p.Stats.Record(rewrite.SurfaceHeader, 0, ev)
			if !p.DryRun && string(out) != v {
				vs[i] = string(out)
				changed = true
			}
		}
	}

	// Set-Cookie Domain= is Tier 1 and load-bearing. ms_cookie_constants()
	// defines COOKIE_DOMAIN from the network domain on any subdomain multisite
	// that does not set it explicitly, so such a site emits Domain=.www.example
	// while the browser is on a variant host — the cookie is discarded and login
	// fails outright. Dropping the attribute is always safe; host-scoped is
	// exactly what a variant host wants.
	if !p.DryRun {
		if cookies := resp.Header.Values("Set-Cookie"); len(cookies) > 0 {
			for i, c := range cookies {
				if nc := p.dropCookieDomain(c); nc != c {
					cookies[i] = nc
					changed = true
				}
			}
		}
	}

	return p.finishBody(resp, st, changed)
}

func (p *Proxy) finishBody(resp *http.Response, st *state, changed bool) error {
	if !rewritableHTML(resp.Header.Get("Content-Type")) {
		return nil
	}
	resp.Body = rewrite.NewHTML(resp.Body, p.Map.Forward(), resp.Body, rewrite.Options{
		DryRun: p.DryRun,
		Stats:  p.Stats,
	})
	// An identity map cannot change a byte, so the upstream's length and
	// validators still describe the body. Dropping them anyway made test 24's
	// premise — that an identity map is a no-op — true of the body but not of
	// the response.
	if !p.Map.Identity() {
		changed = true
	}

	if changed && !p.DryRun {
		// Every rewrite changes body length, so no stale length may be
		// forwarded. ContentLength is cleared alongside the header because
		// flushInterval keys off the struct field, not the header.
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1

		// Drop the validators. Otherwise a conditional request returns 304 and
		// the browser serves a cached canonical-bearing body, defeating test 28
		// silently.
		resp.Header.Del("ETag")
		resp.Header.Del("Last-Modified")
		resp.Header.Del("Accept-Ranges")
	}
	return nil
}

// rewriteRequestBody maps variant origins back to canonical in a write body.
//
// Without it, content saved through wp-admin stores dev hostnames: Gutenberg
// receives variant URLs and sends them straight back (tests 30 and 31).
func (p *Proxy) rewriteRequestBody(r *http.Request, st *state) {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return
	}
	if r.Body == nil || r.Body == http.NoBody {
		return
	}
	ct := r.Header.Get("Content-Type")
	kind := bodyKind(ct)
	if kind == bodyOther {
		return
	}

	max := p.maxBody()
	buf, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return
	}
	if int64(len(buf)) > max {
		// Over the cap: stream through untouched and log the skip. The bytes
		// already read are put back in front of the rest.
		p.log().Warn("request body exceeds the size cap, passing through untouched",
			"cap", max, "content-type", ct)
		p.Stats.Record(rewrite.SurfaceRequestBody, 0, []origin.Event{{
			Surface: rewrite.SurfaceRequestBody, Action: origin.ActionSkipped,
			Reason: origin.ReasonSizeCap,
		}})
		rest := r.Body
		r.Body = readCloser{io.MultiReader(bytes.NewReader(buf), rest), rest}
		return
	}
	_ = r.Body.Close()

	rev := p.Map.Reverse()
	explain := p.Stats.Explain()
	var out []byte
	if kind == bodyMultipart {
		out = rewriteMultipart(buf, ct, rev, p.Stats, explain)
	} else {
		var ev []origin.Event
		out, ev = rev.Rewrite(buf, rewrite.SurfaceRequestBody, explain)
		p.Stats.Record(rewrite.SurfaceRequestBody, 0, ev)
	}
	if p.DryRun {
		out = buf
	}

	st.body = out
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.TransferEncoding = nil
}

// dropCookieDomain removes a Domain= attribute naming a canonical host.
// Third-party cookie domains are left alone.
func (p *Proxy) dropCookieDomain(c string) string {
	parts := strings.Split(c, ";")
	out := parts[:0]
	for _, part := range parts {
		k, v, ok := strings.Cut(part, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), "domain") {
			d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(v)), ".")
			if p.isCanonicalDomain(d) {
				continue // drop the attribute entirely
			}
		}
		out = append(out, part)
	}
	return strings.Join(out, ";")
}

// isCanonicalDomain reports whether d is, or is a parent of, a canonical host.
// A cookie for .herrfors.fi covers www.herrfors.fi and must go too.
func (p *Proxy) isCanonicalDomain(d string) bool {
	if d == "" {
		return false
	}
	for _, s := range p.Map.Sites {
		for _, c := range s.CanonicalSet() {
			if c.Host == d || strings.HasSuffix(c.Host, "."+d) {
				return true
			}
		}
	}
	return false
}

// rewritableHTML reports whether a Content-Type enters the HTML rewriter.
//
// The gate is Content-Type, not a body scan (PLAN §5.2). Learning that a 4 MB
// JPEG contains no canonical host would mean reading 4 MB through the automaton
// — buffering exactly the bodies most worth streaming — when the header answered
// it for free. text/css and application/javascript are deliberately excluded per
// Tier 2: 88 CSS and 185 JS files in the fleet's themes, zero absolute URLs.
func rewritableHTML(ct string) bool {
	return strings.EqualFold(mediaType(ct), "text/html")
}

func mediaType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(ct)
}

type bodyClass int

const (
	bodyOther bodyClass = iota
	bodyFlat
	bodyMultipart
)

func bodyKind(ct string) bodyClass {
	mt := strings.ToLower(mediaType(ct))
	switch {
	case mt == "application/x-www-form-urlencoded",
		mt == "application/json",
		strings.HasSuffix(mt, "+json"),
		strings.HasPrefix(mt, "text/"):
		return bodyFlat
	case mt == "multipart/form-data":
		return bodyMultipart
	}
	return bodyOther
}

// safeMethod reports a method with no side effects, which is the only kind the
// self-redirect carve-out may apply to.
func safeMethod(r *http.Request) bool {
	if r == nil {
		return false
	}
	return r.Method == http.MethodGet || r.Method == http.MethodHead
}

func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// sameURL compares two absolute URLs for the self-redirect guard.
//
// It normalises the default port and an empty path to "/". It does *not* treat
// "/a" and "/a/" as equal — an earlier version of this comment claimed it did.
// If an upstream's hardcoded redirect ever differs from the request by a
// trailing slash the guard will miss it, the Location will be rewritten back to
// the variant, and the request will loop.
func sameURL(a, b string) bool {
	na, ok := normaliseURL(a)
	if !ok {
		return false
	}
	nb, ok := normaliseURL(b)
	return ok && na == nb
}

func normaliseURL(s string) (string, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", false
	}
	o, err := origin.Parse(u.Scheme + "://" + u.Host)
	if err != nil {
		return "", false
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	q := u.RawQuery
	if q != "" {
		q = "?" + q
	}
	return o.HostPort() + path + q, true
}

// blank replaces a response with a plain-text status.
func blank(resp *http.Response, code int, msg string) {
	resp.StatusCode = code
	resp.Status = http.StatusText(code)
	resp.Header = http.Header{
		"Content-Type": []string{"text/plain; charset=utf-8"},
	}
	resp.Body = io.NopCloser(strings.NewReader(msg))
	resp.ContentLength = int64(len(msg))
}

type readCloser struct {
	io.Reader
	io.Closer
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
