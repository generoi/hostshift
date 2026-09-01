// Package corpus implements the corpus diff — PLAN §7's "only test that
// validates against reality".
//
// Fixtures would not have caught the double-port bug. This would: it crawls N
// URLs on the canonical site and the same N through the proxy, rewrites the
// canonical bytes through the same engine the proxy uses, and compares.
package corpus

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// Options configures a run.
type Options struct {
	Canonical *url.URL // the base the database was written for
	Variant   *url.URL // the base the browser uses
	Map       *origin.Map
	N         int           // how many pages to crawl
	Timeout   time.Duration // per request
	Paths     []string      // explicit paths; when empty, crawl from "/"
	Client    *http.Client

	// StrictOrigins mirrors the proxy flag of the same name: with the
	// self-redirect carve-out turned off there, an unchanged Location is a
	// mismatch here too.
	StrictOrigins bool

	// CanonicalHeaders are added to the canonical fetch only.
	//
	// When the canonical base is resolved past the TLS-terminating router
	// straight to the container, the headers that router would have added are
	// missing — and X-Forwarded-Proto in particular is load-bearing: without it
	// WordPress believes the request is plain HTTP and canonical-redirects to
	// the https URL it is already on. Comparing against that redirect measures
	// nothing. The variant side needs no such help, because hostshift adds the
	// header itself.
	CanonicalHeaders map[string]string

	// Resolve overrides DNS, "host:port" → "addr:port", the way curl's
	// --resolve does.
	//
	// It is not a convenience. Under production-canonical the canonical base
	// *is* the production hostname, so running the diff without it would crawl
	// the client's live site — the one thing this whole design exists to keep
	// the developer away from. Resolving it to the local container makes the
	// target explicit.
	Resolve map[string]string
}

// Result is one page's comparison.
type Result struct {
	Path string
	// Equal reports byte equality between the rewritten canonical page and the
	// page the proxy served.
	Equal bool
	// Tier2 counts canonical origins in a body the proxy is documented not to
	// rewrite — `text/css` and the JavaScript types. PLAN's fast path says they
	// are "added only if the corpus diff shows a leak", so a non-zero count here
	// is this tool's designed trigger for adding them rather than a defect.
	Tier2 int

	// ContentType is what the variant response was labelled, because the proxy
	// dispatches on it and a verdict that ignores it is scoring a body against a
	// pipeline that never ran.
	ContentType string

	// BrokenSerialized counts PHP-serialized values in the variant response
	// whose declared length does not describe their data. PHP refuses those, or
	// worse, truncates them silently and keeps parsing.
	BrokenSerialized int
	// UnreadRewrites counts spans the rewrite changed inside something
	// serialized-shaped that no spelling could read. See rewrite.UnreadRewrites:
	// it reports what BrokenSerialized cannot, because it is host-dependent and
	// so does not cancel against the canonical baseline.
	UnreadRewrites int

	// Leaks counts canonical origins in the variant response. Any non-zero
	// value is a test 28 failure and is what this whole exercise is for.
	Leaks int
	// LinesCanonical and LinesVariant are close even when bytes are not:
	// splicing never rebuilds whitespace. They are not identical — the two
	// fetches carry different Host headers, and WordPress emits Host-dependent
	// markup — so a small delta is reported and a large one is fatal, per
	// hostDependentLines.
	LinesCanonical, LinesVariant int
	// BytesCanonical and BytesVariant are the same question in a unit every
	// document has. Lines are not: minified HTML — WP Rocket, Autoptimize,
	// LiteSpeed and Cloudflare's minifier all emit one — and every JSON body
	// count zero lines however much or little is in them, so for that whole
	// class the line counts were equal by construction and the size bound never
	// ran at all.
	BytesCanonical, BytesVariant int
	DiffLines                    int
	Err                          error
}

// Run crawls and compares.
func Run(ctx context.Context, o Options) ([]Result, error) {
	if o.Client == nil {
		timeout := o.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if len(o.Resolve) > 0 {
			base := &net.Dialer{Timeout: timeout}
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				// The keys are folded where they are built, by ResolveKey, and
				// net/http hands this the punycode host lowercased — so folding
				// again here is unmeasurable, and an unmeasurable guard is one
				// nobody can tell has stopped working. Keying both sides through
				// one function is the fix; doing it twice is not more of it.
				if to, ok := o.Resolve[addr]; ok {
					addr = to
				}
				return base.DialContext(ctx, network, addr)
			}
			// The certificate will be the container's mkcert one, which carries
			// no production name. Verifying it is not what this test is about;
			// test 29b is where the TLS behaviour is asserted.
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
		o.Client = &http.Client{
			Timeout:   timeout,
			Transport: tr,
			// Redirects are part of what is being compared, not something to
			// follow past.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	paths := o.Paths
	if len(paths) == 0 {
		var err error
		if paths, err = crawl(ctx, o); err != nil {
			return nil, err
		}
	}
	if o.N > 0 && len(paths) > o.N {
		paths = paths[:o.N]
	}

	results := make([]Result, 0, len(paths))
	for _, p := range paths {
		results = append(results, compare(ctx, o, p))
	}
	return results, nil
}

func compare(ctx context.Context, o Options, path string) Result {
	r := Result{Path: path}

	canon, err := fetch(ctx, o, o.Canonical, path)
	if err != nil {
		r.Err = fmt.Errorf("canonical: %w", err)
		return r
	}
	variant, err := fetch(ctx, o, o.Variant, path)
	if err != nil {
		r.Err = fmt.Errorf("variant: %w", err)
		return r
	}

	// A redirect verifies nothing about the body, and its Location is the header
	// this design worries about most.
	//
	// Comparing bodies alone scored an all-redirect crawl as "3 pages, 3
	// byte-identical, 0 leaks — GREEN" with hostshift not in the path at all,
	// because two empty bodies are equal. The shapes that produce such a crawl
	// are the documented ones: a worktree whose database is empty redirects every
	// page to install.php, and a login-walled preview does the same. The README
	// calls this the check that validates a deployment against reality.
	if canon.status != variant.status {
		r.Err = fmt.Errorf("status %d canonical, %d variant", canon.status, variant.status)
		return r
	}
	if canon.location != "" || variant.location != "" {
		wantLoc, _ := o.Map.Forward().Rewrite([]byte(canon.location), "header", false)
		// The self-redirect carve-out is not a mismatch. PLAN §4.4 and test 32
		// enumerate it as correct: an asset the worktree does not have is
		// redirected to the canonical origin *on purpose*, which is what
		// redirect-uploads.conf does in 87% of the fleet with 95.2% of referenced
		// uploads absent locally. Flagging it made a RED verdict the ordinary
		// outcome on any page linking a PDF or an attachment, which is how a
		// verdict stops being read.
		//
		// Under --strict-origins the guard is off in the proxy too, so the
		// exemption goes with it.
		//
		// And it is the *proxy's* guard, which asks whether rewriting the
		// Location would yield the URL the browser just requested — PLAN §4.4's
		// wording, and `sameURL(rewritten, st.url)` in modifyResponse. This
		// asked only whether the Location came back unchanged, which is a
		// strictly wider question and, worse, is the exact signature of the
		// failure it is meant to sit beside: an unrewritten canonical Location
		// is byte-identical on both sides *by construction*. So the check
		// switched itself off precisely when it was needed. An all-redirect
		// crawl with hostshift out of the path — the case the comment above
		// records this Location comparison as having been added to catch — was
		// GREEN again, and so was a login redirect that PLAN test 32 names as
		// one the guard must not cover.
		unchangedSelfRedirect := !o.StrictOrigins &&
			variant.location == canon.location && canon.location != "" &&
			redirectsToItself(string(wantLoc), o, path)
		if string(wantLoc) != variant.location && !unchangedSelfRedirect {
			r.Err = fmt.Errorf("Location %q, want %q", variant.location, wantLoc)
			return r
		}
		// A redirect with a matching Location and no body is verified; one with a
		// body still has its body compared below.
	}
	if len(canon.body) == 0 && len(variant.body) == 0 && canon.location == "" {
		r.Err = fmt.Errorf("empty body and no Location: nothing was verified")
		return r
	}

	// The canonical bytes through the same arm the proxy would run for this
	// content type — not the HTML pipeline regardless, which made `want`
	// byte-identical to a leaking XML body and scored it "same".
	want, err := applyLikeTheProxy(o.Map.Forward(), canon.body, variant.contentType, nil)
	if err != nil {
		r.Err = err
		return r
	}

	r.Equal = string(want) == string(variant.body)
	r.LinesCanonical = strings.Count(string(want), "\n")
	r.LinesVariant = strings.Count(string(variant.body), "\n")
	r.BytesCanonical = len(want)
	r.BytesVariant = len(variant.body)
	r.DiffLines = countDiffLines(string(want), string(variant.body))

	// The safety-critical assertion, independent of byte equality: a live site
	// differs between two fetches for a dozen innocent reasons (nonces,
	// timestamps, ad slots), but a canonical origin reaching the browser is
	// never innocent.
	//
	// Counted by running the matcher, not by counting two spellings. Looking for
	// "//host/" and "\/\/host\/" missed a homepage link (https://host"), a
	// query-only URL (https://host?w=1) and every percent-encoded form — so the
	// one test §7 calls "the only test that validates against reality" reported
	// GREEN on the leak class the prefilter bug produced. The matcher is by
	// definition exactly the set of origins the proxy claims to rewrite, which
	// makes this assertion say what it means: anything it still finds in the
	// variant body is one the proxy should have caught and did not.
	// Serialized payloads the browser is served must still parse. This is the
	// only assertion here that does not compare the two sides: when the proxy
	// and the scorer are wrong in the same way, comparison says nothing, and
	// that is exactly how five rounds of silent wp_options destruction went
	// unreported by the run PLAN §7 calls the only test that validates against
	// reality.
	// Against the canonical baseline, so this blames the proxy for what the
	// proxy did. Real WordPress databases carry broken serialized rows already —
	// from the careless search-replace hostshift exists to avoid — and counting
	// the variant alone made every such site RED forever, on bytes the proxy had
	// passed through untouched.
	r.BrokenSerialized = rewrite.BrokenSerialized(variant.body) -
		rewrite.BrokenSerialized(canon.body)
	if r.BrokenSerialized < 0 {
		r.BrokenSerialized = 0
	}

	// And the spellings the walk cannot read at all, which BrokenSerialized is
	// structurally unable to report: a value it cannot read does not parse on
	// the canonical page either, so the subtraction above cancels it to zero.
	//
	// This asks the other question — did the rewrite change bytes inside
	// something serialized-shaped that no spelling accounted for — which is
	// host-dependent, so it is zero on the canonical side by construction and
	// survives the same subtraction.
	// Through the same pipeline countLeaks uses, not the bare byte matcher.
	// Asking Matcher.Rewrite alone is the mistake countLeaks' own comment
	// records — obfuscated separators, folded hosts, CSS escapes and character
	// references are invisible to it by construction, so a host spelled any of
	// those ways was rewritten by the proxy and reported as untouched here.
	//
	// No content-type guard of its own. An attachment and a Tier 2 body are ones
	// the proxy deliberately does not rewrite, so applyLikeTheProxy returns them
	// unchanged and the "did the rewrite touch this" test below answers no —
	// which is the same answer a guard would give, from the property that makes
	// it true rather than from a second list that could drift from the first.
	{
		if rewrite.UnreadSerialized(canon.body, func(b []byte) []byte {
			out, err := applyLikeTheProxy(o.Map.Forward(), b, canon.contentType, nil)
			if err != nil {
				return b
			}
			return out
		}) {
			r.UnreadRewrites = 1
		}
	}

	r.ContentType = variant.contentType
	r.Leaks, r.Tier2 = countLeaks(o.Map.Forward(), variant)
	return r
}

// countLeaks reports how many canonical origins the matcher still finds in a
// body that has already been through the proxy.
// countLeaks asks the whole engine, not the byte matcher alone.
//
// It used to run `m.Rewrite` and justify that with "the matcher is by definition
// exactly the set of origins the proxy claims to rewrite". That stopped being
// true the moment urlobf.go existed: the proxy also runs the URL-parser view,
// the IDNA fold, the CSS view and the reference views, and this ran none of
// them. So every leak class found since — obfuscated separators, folded hosts,
// CSS escapes, character references — was invisible by construction to the one
// test §7 calls the only one that validates against reality, and it printed
// GREEN on a page whose `<a href>` a real browser resolved to production.
//
// Pushing the served bytes back through the same pipeline the proxy runs answers
// the actual question: anything it still finds to rewrite is an origin that
// should already have been rewritten and was not.
// The second return is the Tier 2 count: origins in a body the proxy is
// *documented* not to rewrite. PLAN's fast-path section excludes `text/css` and
// the JavaScript types "per Tier 2, and added only if the corpus diff shows a
// leak" — so finding one there is this tool doing its job, and reporting it as
// a proxy defect would be reporting the wrong thing. An attachment is different
// again: §5 skips it by design, whatever it contains, so it is not counted at
// all. Scoring every body through the HTML pipeline made a PDF or a WooCommerce
// download link — which the `<a href>` crawler reaches routinely — read as
// CANONICAL ORIGIN REACHED THE BROWSER.
// redirectsToItself reports whether loc — the *rewritten* Location — is the URL
// the browser asked for. That is the proxy's self-redirect test, asked from the
// outside.
//
// Against a *variant origin from the map*, not the fetch base. Those differ:
// `--variant-base` and `--resolve` exist so the crawl can be pointed somewhere
// else, and the URL the browser would have used is the one the map names.
//
// Host comparison is case-insensitive and ignores the scheme, like the proxy's:
// a router that terminates TLS turns an https request into an http one
// upstream, and the guard has to recognise its own redirect through that.
func redirectsToItself(loc string, o Options, path string) bool {
	u, err := url.Parse(loc)
	if err != nil || u.Host == "" {
		return false
	}
	want, err := url.Parse("https://x" + path)
	if err != nil {
		return false
	}
	if u.EscapedPath() != want.EscapedPath() || u.RawQuery != want.RawQuery {
		return false
	}
	// *The* variant being crawled, not any variant in the map.
	//
	// Accepting any of them exempted a redirect from one site in a multisite
	// map to another — `www.b.fi` 301ing every path to `www.a.fi` is an
	// ordinary consolidation redirect, and the browser follows it to production.
	// The proxy's guard is `sameURL(rewritten, st.url)` against the single URL
	// the browser asked for; there is only ever one.
	//
	// HostPort, not Host: an Origin keeps its port in a separate field, and the
	// map is origin→origin — scheme, host *and* port. Comparing the host alone
	// meant a variant on a non-default port could never match its own
	// self-redirect, so every page linking an upload went RED on a deployment
	// doing exactly what §4.4 prescribes.
	crawled := o.Variant
	for _, site := range o.Map.Sites {
		if crawled != nil && strings.EqualFold(crawled.Host, site.Variant.HostPort()) {
			return strings.EqualFold(u.Host, site.Variant.HostPort())
		}
	}
	// The crawl is pointed somewhere that is not a variant in the map — a
	// `--variant-base` override, or a test harness. Fall back to the primary,
	// which is what both bases default to.
	if len(o.Map.Sites) > 0 {
		return strings.EqualFold(u.Host, o.Map.Sites[0].Variant.HostPort())
	}
	return false
}

// ResolveKey normalises a host:port for the --resolve map, so the guardrail that
// decides whether to warn and the dialer that decides where to connect cannot
// disagree about what host they are looking at.
func ResolveKey(hostPort string) string {
	h, p, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort
	}
	n, err := origin.NormaliseHost(h)
	if err != nil {
		n = strings.ToLower(h)
	}
	return net.JoinHostPort(n, p)
}

func countLeaks(m *origin.Matcher, r response) (leaks, tier2 int) {
	if r.attachment {
		return 0, 0
	}
	if isTier2(r.contentType) {
		// Scanned with the text arm on purpose. The proxy does nothing to these
		// types, so asking "what would the proxy have done" answers "nothing" —
		// and the whole point of the Tier 2 count is to find the origins it is
		// leaving behind, which is PLAN's stated trigger for adding them.
		return 0, originsIn(m, r.body, "text/plain")
	}
	return originsIn(m, r.body, r.contentType), 0
}

// isTier2 reports whether the proxy deliberately leaves this type alone.
func isTier2(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	}
	switch mt {
	case "text/css", "application/javascript", "text/javascript",
		"application/x-javascript", "application/ecmascript", "text/ecmascript":
		return true
	}
	return false
}

// originsIn asks the whole engine, not the byte matcher alone.
//
// It used to run `m.Rewrite` and justify that with "the matcher is by definition
// exactly the set of origins the proxy claims to rewrite". That stopped being
// true the moment urlobf.go existed: the proxy also runs the URL-parser view,
// the IDNA fold, the CSS view and the reference views, and this ran none of
// them. So every leak class found since — obfuscated separators, folded hosts,
// CSS escapes, character references — was invisible by construction to the one
// test §7 calls the only one that validates against reality, and it printed
// GREEN on a page whose `<a href>` a real browser resolved to production.
func originsIn(m *origin.Matcher, body []byte, ct string) int {
	st := rewrite.NewStats(false)
	out, err := applyLikeTheProxy(m, body, ct, st)
	if err != nil {
		return 0
	}
	if n := st.Total(); n > 0 {
		return n
	}
	// A pass that splices without recording — and one that changes bytes has
	// found an origin whatever it counted.
	if string(out) != string(body) {
		return 1
	}
	return 0
}

// applyLikeTheProxy runs the arm the proxy would run for this content type.
//
// This ran NewResponseBody — the HTML pipeline, XMLEntities off — on every body,
// while proxy.go dispatches every `*xml` media type to HostLeaksXMLCounted,
// which applies the reference and CSS views over the whole buffer. The HTML
// pipeline applies the reference view only where an *HTML* parser decodes one:
// attributes and foreign content. Element content in an ordinary XML element is
// the gap — and that is where every sitemap `<loc>` and every RSS `<link>`
// lives. So the one test PLAN §7 calls "the only test that validates against
// reality" scored an unrewritten feed GREEN, and the byte-equality half
// positively rewarded the leak, because `want` was computed the same blind way.
func applyLikeTheProxy(m *origin.Matcher, body []byte, ct string, st *rewrite.Stats) ([]byte, error) {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	}
	mt = strings.ToLower(mt)
	switch {
	case mt == "text/html" || mt == "application/xhtml+xml":
		return io.ReadAll(rewrite.NewResponseBody(strings.NewReader(string(body)), m, nil,
			rewrite.Options{Stats: st, XMLEntities: mt == "application/xhtml+xml"}))

	// Ahead of the XML arm, because `application/ld+json` ends in neither and
	// the proxy tests JSON first.
	case mt == "application/json", mt == "text/json", strings.HasSuffix(mt, "+json"):
		out := rewrite.RewriteJSON(body, m, st, nil, false)
		// Inside the repair: the sweep is a raw byte matcher, so a host it
		// rewrites inside a serialized string leaves the length stale. On
		// RewriteJSON's decline path — a duplicate member is legal JSON and
		// is rejected — the sweep is the only pass that touches the body, so
		// it corrupted the blob while logging a line that reads like a save.
		return rewrite.RepairSerialized(out, func(b []byte) []byte {
			return rewrite.SweepBytes(b, m, st, nil)
		}), nil

	// The enumerated set plus `+xml`, exactly as rewritableText has it — not
	// `HasSuffix(mt, "xml")`, which also swallows text/xml-external-parsed-entity
	// and application/vnd.foo.xml, and not `HasPrefix(mt, "text/")`, which
	// swallows text/markdown. Either one made the scorer rewrite a body the
	// proxy passes through, and the run went RED on a healthy deployment.
	case isTextArm(mt):
		// The proxy's `{`/`[` sniff first. wp-admin/async-upload.php sets
		// text/plain before wp_send_json can set application/json, so the body
		// that reports every media upload arrives on this arm as JSON — and
		// wp_json_encode writes its origins with \uXXXX escapes, which only
		// RewriteJSON decodes. Without the sniff the scorer served that body
		// back unrewritten and called the page clean.
		if t := bytes.TrimLeft(body, " \t\r\n"); len(t) > 0 && (t[0] == '{' || t[0] == '[') {
			out := rewrite.RewriteJSON(body, m, st, nil, false)
			// Inside the repair: the sweep is a raw byte matcher, so a host it
			// rewrites inside a serialized string leaves the length stale. On
			// RewriteJSON's decline path — a duplicate member is legal JSON and
			// is rejected — the sweep is the only pass that touches the body, so
			// it corrupted the blob while logging a line that reads like a save.
			return rewrite.RepairSerialized(out, func(b []byte) []byte {
				return rewrite.SweepBytes(b, m, st, nil)
			}), nil
		}
		// All three passes the proxy runs, in order. Running only the middle one
		// scored a plain, unencoded, dereferenceable origin as clean: stripForURL
		// *deletes* tab, LF and CR — right for a single URL value, wrong for a
		// whole document, where those bytes are token separators. Removing the
		// newline welds the previous word onto `https:`, tokenBoundary is then
		// false, and no candidate is emitted. The byte matcher and the sweep,
		// which the proxy runs and this did not, see the raw bytes.
		// Wrapped in RepairSerialized, exactly as proxy.go's text arm is. Both
		// were edited in the same commit and only one got the wrapper, so the
		// scorer disagreed with the proxy on any body carrying an `s:N:"…"` —
		// which sends the run spuriously RED on a real page.
		var ev []origin.Event
		out := rewrite.RepairSerialized(body, func(b []byte) []byte {
			nv, nev := m.RewriteText(b, rewrite.SurfaceText, false)
			ev = append(ev, nev...)
			if strings.HasSuffix(mt, "xml") {
				return rewrite.HostLeaksXMLCounted(m, nv, false, st, rewrite.SurfaceText, 0)
			}
			return rewrite.HostLeaksCounted(m, nv, false, st, rewrite.SurfaceText, 0)
		})
		st.Record(rewrite.SurfaceText, 0, ev)
		// Inside the repair: the sweep is a raw byte matcher, so a host it
		// rewrites inside a serialized string leaves the length stale. On
		// RewriteJSON's decline path — a duplicate member is legal JSON and
		// is rejected — the sweep is the only pass that touches the body, so
		// it corrupted the blob while logging a line that reads like a save.
		return rewrite.RepairSerialized(out, func(b []byte) []byte {
			return rewrite.SweepBytes(b, m, st, nil)
		}), nil
	}
	// Everything else the proxy streams through untouched, so scoring it as a
	// page would report a leak on a type it never claimed to rewrite.
	return body, nil
}

// isTextArm is proxy.rewritableText, and the two must not drift —
// TestTheScorerMatchesTheProxy asserts they have not.
func isTextArm(mt string) bool {
	switch mt {
	case "text/plain", "text/xml", "application/xml",
		"application/rss+xml", "application/atom+xml", "image/svg+xml":
		return true
	}
	return strings.HasSuffix(mt, "+xml")
}

// response is what a comparison needs: the body, and the parts of the response
// that decide whether the body means anything.
type response struct {
	body     []byte
	status   int
	location string
	// The proxy dispatches on Content-Type and Content-Disposition, so a
	// verdict that ignores them is scoring a body against a pipeline that never
	// ran on it.
	contentType string
	attachment  bool
}

func fetch(ctx context.Context, o Options, base *url.URL, path string) (response, error) {
	u := *base
	ref, err := url.Parse(path)
	if err != nil {
		return response{}, err
	}
	u.Path, u.RawQuery = ref.Path, ref.RawQuery

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return response{}, err
	}
	// Ask for identity so the comparison is over the bytes the rewriter saw.
	req.Header.Set("Accept-Encoding", "identity")
	if base == o.Canonical {
		for k, v := range o.CanonicalHeaders {
			req.Header.Set(k, v)
		}
	}
	res, err := o.Client.Do(req)
	if err != nil {
		return response{}, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return response{
		body:        b,
		status:      res.StatusCode,
		location:    res.Header.Get("Location"),
		contentType: res.Header.Get("Content-Type"),
		attachment:  strings.HasPrefix(strings.ToLower(strings.TrimSpace(res.Header.Get("Content-Disposition"))), "attachment"),
	}, err
}

// crawl collects same-host paths from the canonical site, breadth first.
func crawl(ctx context.Context, o Options) ([]string, error) {
	seen := map[string]bool{"/": true}
	queue := []string{"/"}
	var out []string

	// A page's links carry the hosts the *database* holds, which are the map's
	// canonical hosts — not necessarily the host being fetched from. They differ
	// whenever --canonical-base points somewhere else, which is exactly how this
	// is run against a local canonical site rather than against production.
	sameSite := map[string]bool{strings.ToLower(o.Canonical.Hostname()): true}
	for _, s := range o.Map.Sites {
		for _, c := range s.CanonicalSet() {
			sameSite[c.Host] = true
		}
	}

	// Pages before subresources.
	//
	// `-n` is a number of pages, and one FIFO made it a number of *fetches*. A
	// `<head>` is emitted before the `<body>`, so every stylesheet and script
	// enqueued ahead of the first `<a href>`: measured on a page with 15
	// stylesheets, 15 scripts and 10 links, `-n 20` fetched the homepage and
	// nineteen files from its head, and not one other page. The trade is not
	// neutral — Tier 2 is a note and never fails a run, so that spent nineteen
	// slots of the only check in the tool that goes RED for invariant 28 to make
	// one note louder.
	//
	// They are still fetched — the Tier 2 count they exist for is read off a
	// response body, so it needs a request — but they go behind every page the
	// budget can reach, not in front of them. Ordering is the whole defect:
	// nothing about reaching a stylesheet requires reaching it first.
	var subQueue []string
	enqueue := func(link, from string, isDoc bool) {
		u, err := url.Parse(link)
		if err != nil || u.Path == "" {
			return
		}
		// Same site only: a crawl that wanders onto a third-party host is
		// measuring nothing.
		if u.Host != "" && !sameSite[strings.ToLower(u.Hostname())] {
			return
		}
		if u.Fragment != "" && u.Path == from {
			return
		}
		key := u.Path
		if u.RawQuery != "" {
			key += "?" + u.RawQuery
		}
		if seen[key] {
			return
		}
		seen[key] = true
		if isDoc {
			queue = append(queue, key)
			return
		}
		subQueue = append(subQueue, key)
	}

	for (len(queue) > 0 || len(subQueue) > 0) && (o.N == 0 || len(out) < o.N) {
		// Drain every page first, and only then the subresources they named. A
		// single FIFO put them in document order, and a `<head>` comes before a
		// `<body>` — so every stylesheet and script was enqueued ahead of the
		// first `<a href>`.
		var p string
		if len(queue) > 0 {
			p, queue = queue[0], queue[1:]
		} else {
			p, subQueue = subQueue[0], subQueue[1:]
		}
		out = append(out, p)

		res, err := fetch(ctx, o, o.Canonical, p)
		if err != nil {
			continue
		}
		docs, subs := links(res.body)
		for _, link := range docs {
			enqueue(link, p, true)
		}
		for _, link := range subs {
			enqueue(link, p, false)
		}
	}
	sort.Strings(out)
	return out, nil
}

// links collects what the crawl should fetch next: the pages a reader can reach,
// and the subresources the browser fetches whether or not anyone clicks.
//
// `<a href>` alone meant the crawl never fetched a stylesheet, and the Tier 2
// line — the one PLAN's fast path names as its trigger for rewriting CSS — could
// not fire from a default run. Measured: a page linking its own
// `<link rel=stylesheet>` whose file carried a live production origin scored
// "3 pages, 0 leaks" and GREEN, while `curl` on that stylesheet through the
// proxy returned the canonical URL. The README points at this command for
// exactly that case, so the evidence it asks for was unreachable by it.
//
// Subresources are followed, not just noted, because the point is to *fetch*
// them: `Result.Tier2` is counted from a response body, which requires a request.
func links(body []byte) (docs, subs []string) {
	z := html.NewTokenizer(strings.NewReader(string(body)))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return docs, subs
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		name, hasAttr := z.TagName()
		// The attribute carrying a URL differs by element, and taking `href`
		// from everything would pull in `<base href>` — which is not a
		// document — and every `<link rel=alternate>` to another site.
		var want string
		doc := false
		switch string(name) {
		case "a":
			want, doc = "href", true
		case "link":
			want = "href"
		case "script", "img", "iframe":
			want = "src"
		default:
			continue
		}
		for hasAttr {
			var k, v []byte
			k, v, hasAttr = z.TagAttr()
			if string(k) != want {
				continue
			}
			if doc {
				docs = append(docs, string(v))
			} else {
				subs = append(subs, string(v))
			}
		}
	}
}

func countDiffLines(a, b string) int {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	n := 0
	for i := 0; i < len(la) || i < len(lb); i++ {
		var x, y string
		if i < len(la) {
			x = la[i]
		}
		if i < len(lb) {
			y = lb[i]
		}
		if x != y {
			n++
		}
	}
	return n
}

// WriteReport summarises a run. It returns true when the run is green: no
// canonical origin reached the browser, no served value was re-serialised past
// what PHP will read, and no page lost or gained enough lines to be a different
// document.
// hostDependentLines: how many lines of a document may differ between the two
// fetches purely because they carry different Host headers. WordPress emits one
// `<link rel="dns-prefetch">` per asset host that is not SERVER_NAME, and a
// multisite parent can have a few asset hosts; nothing plausible emits eight.
// Truncation loses far more, which is the point of the bound.
const hostDependentLines = 8

func WriteReport(w io.Writer, results []Result) bool {
	// A run that compared nothing is not a run that found nothing. `green` is
	// only ever cleared by a result, so an empty slice — a negative `-n`, a
	// `--paths` file of comments — walked no rows, cleared nothing, and printed
	// the invariant-28 verdict with exit 0. The same class as the two verdicts
	// round 43 rescoped: a report asserting what it skipped.
	green := len(results) > 0
	var equal, leaks, errs, tier2, broken, unread int

	fmt.Fprintf(w, "%-46s %-8s %-7s %-7s %s\n", "PATH", "BYTES", "LEAKS", "LINES", "NOTE")
	for _, r := range results {
		// Every note a page has earned, not the first one. This was a switch, so
		// a page that both leaked an origin *and* served a blob PHP refuses
		// reported only the leak — and the page most likely to do both is the
		// one carrying a serialized payload full of URLs, which is the shape
		// this whole detector exists for.
		var notes []string
		if r.Err != nil {
			notes, errs = append(notes, r.Err.Error()), errs+1
			green = false
		}
		if r.Leaks > 0 {
			notes = append(notes, "CANONICAL ORIGIN REACHED THE BROWSER")
			leaks += r.Leaks
			green = false
		}
		if r.UnreadRewrites > 0 {
			notes = append(notes, "a serialized value here was rewritten in an encoding "+
				"this build cannot read, so no length was re-emitted — look at this page")
			unread++
			green = false
		}
		if r.BrokenSerialized > 0 {
			notes = append(notes, fmt.Sprintf("%d serialized value(s) served with "+
				"a length that does not describe the data — PHP will refuse or "+
				"truncate them", r.BrokenSerialized))
			broken += r.BrokenSerialized
			green = false
		}
		if r.Tier2 > 0 {
			// Not a defect: PLAN's fast path excludes these types "per Tier 2,
			// and added only if the corpus diff shows a leak". This is that
			// showing. It does not turn the run RED, because the proxy is doing
			// what it says it does — but it is the trigger, so it is loud.
			notes = append(notes, fmt.Sprintf("%d origins in a Tier 2 type (%s) — "+
				"the PLAN's trigger for rewriting it", r.Tier2, r.ContentType))
			tier2 += r.Tier2
		}
		if r.LinesCanonical != r.LinesVariant {
			// Reported, not fatal.
			//
			// This was a proxy for "something re-serialised" from before there
			// was a direct test for it. There is now: `broken` asks PHP's own
			// question of the served bytes, and `unread` names a value the walk
			// could not read. Both are exact where this is an inference.
			//
			// And under production-canonical the inference is simply wrong. The
			// canonical hostname is not routed locally — that is the whole point
			// of the mode — so the canonical fetch carries a different Host than
			// the proxy sends upstream, and WordPress emits one extra
			// `<link rel="dns-prefetch">` for every asset host that is not
			// SERVER_NAME. Exactly one line, on every page, on a site with
			// nothing wrong with it. Measured: 1825/1824 across a whole stock
			// Bedrock crawl, every row RED.
			//
			// A verdict that is red on every healthy page in the mode its own
			// README calls "where the hazards live" is one nobody reads, and on
			// the run that did carry 32 broken values the real signal was a
			// clause appended to a phrase that fires regardless.
			//
			// Demoted, but not deleted: bounded instead. Nothing else in this
			// report says anything about the *size* of what the proxy served.
			// `Err` needs a transport failure, `Leaks` needs an origin to
			// survive, `broken` and `unread` need a serialized value to be
			// present to be wrong about — so an upstream that answers 200 with
			// an empty body, or dies mid-stream and serves half the document,
			// satisfies every one of them. Dropping the assertion outright
			// scored both GREEN, under a verdict line that goes on saying "no
			// page re-serialised".
			//
			// Two bounds, because the two failures have different shapes. A
			// Host-dependent line is a handful of lines in a document of any
			// size, so more than `hostDependentLines` of them is not that. And
			// on a short page a handful *is* the document, so a variant that
			// lost or gained a quarter of it is not the same page either.
			// Measured 1825/1824 passes both; an empty body and a body cut to
			// five lines of eight fail one each.
			d := r.LinesCanonical - r.LinesVariant
			if d < 0 {
				d = -d
			}
			if d > hostDependentLines || d*4 > r.LinesCanonical {
				green = false
				notes = append(notes, fmt.Sprintf(
					"line count %d→%d — too much of the page to be a Host-dependent "+
						"line; the proxy served a different document",
					r.LinesCanonical, r.LinesVariant))
			} else {
				notes = append(notes, fmt.Sprintf(
					"line count %d→%d — a Host-dependent line like dns-prefetch does this; "+
						"`broken` and `unread` are the tests for re-serialisation",
					r.LinesCanonical, r.LinesVariant))
			}
		} else if len(notes) == 0 && !r.Equal {
			notes = append(notes, fmt.Sprintf("%d lines differ (dynamic content?)", r.DiffLines))
		}

		// The same question in bytes, asked unconditionally.
		//
		// The bound above is expressed entirely in newlines, and a document with
		// none counts zero lines however much is in it — so for minified HTML
		// (WP Rocket, Autoptimize, LiteSpeed and Cloudflare's minifier all emit
		// one line) and for every JSON body the two counts are equal by
		// construction, the branch above is never entered, and the size
		// assertion never ran at all. An upstream answering 200 with nothing in
		// it scored "1 lines differ (dynamic content?)" and GREEN, under a
		// verdict line claiming every page was the length it should be.
		//
		// Outside both branches rather than inside one, because a page can have
		// earned a note already — a Tier 2 origin, a self-redirect — and being
		// half-served is not less true for it.
		//
		// A quarter again, and for the same reason: rewriting changes lengths,
		// since a variant hostname is not the length of the canonical it
		// replaces, but it changes them by a few bytes per URL and never by a
		// quarter of the document.
		db := r.BytesCanonical - r.BytesVariant
		if db < 0 {
			db = -db
		}
		if db*4 > r.BytesCanonical {
			green = false
			notes = append(notes, fmt.Sprintf(
				"%d→%d bytes — too much of the page to be rewriting; the proxy "+
					"served a different document",
				r.BytesCanonical, r.BytesVariant))
		}
		note := strings.Join(notes, "; ")
		// Counted outside the switch. `equal++` used to sit in its last arm, so
		// a page that *is* byte-identical but carries a note was never counted:
		// the BYTES column said `same` while the summary said `0 byte-identical`,
		// which is the line a developer reads.
		if r.Equal {
			equal++
		}
		state := "differ"
		if r.Equal {
			state = "same"
		}
		fmt.Fprintf(w, "%-46s %-8s %-7d %-7s %s\n",
			truncate(r.Path, 46), state, r.Leaks,
			fmt.Sprintf("%d/%d", r.LinesCanonical, r.LinesVariant), note)
	}

	// `broken` belongs on this line. Without it a run could print
	// "3 pages, 3 byte-identical, 0 leaks, 0 errors" and then "corpus diff RED",
	// naming nothing that was wrong — and this summary is what a developer
	// reads before deciding whether to scroll up.
	fmt.Fprintf(w, "\n%d pages, %d byte-identical, %d leaks, %d broken, %d unread, %d errors\n",
		len(results), equal, leaks, broken, unread, errs)
	if tier2 > 0 {
		fmt.Fprintf(w, "%d origins in Tier 2 types (text/css, JavaScript), which the "+
			"proxy excludes by design — PLAN's fast path adds them \"only if the "+
			"corpus diff shows a leak\", and this is that\n", tier2)
	}
	// Whether anything in this run is a type the leak scan actually reads. "At
	// least one row" is weaker than the verdict's sentence: a table of nothing
	// but Tier 2 rows has had no scan run on it, so twenty byte-identical
	// stylesheets printed "no canonical origin reached the browser" over bytes
	// nobody looked at. Not a reason to fail the run — Tier 2 must not, and
	// TestATier2BodyTheProxyNeverRewritesIsNotAnUnreadRewrite holds that — but a
	// reason not to claim the invariant was tested.
	scanned := false
	for _, r := range results {
		if !isTier2(r.ContentType) {
			scanned = true
			break
		}
	}
	switch {
	case green && !scanned:
		fmt.Fprintf(w, "corpus diff GREEN, but nothing in this run is a type the proxy "+
			"rewrites:\n  no canonical origin was looked for, and %d did reach it in Tier 2 "+
			"types.\n  Crawl a page, not only its assets.\n", tier2)
	case green && tier2 > 0:
		// The verdict has to be scoped to what was actually asked.
		//
		// "no canonical origin reached the browser" was printed unconditionally,
		// two lines under a count of origins that had just reached the browser
		// in an excluded type — a production-canonical run with live URLs inside
		// Elementor CSS said GREEN and exited 0. The exclusion is designed
		// (PLAN §5.2 Tier 2) and stays; what could not stay is a sentence
		// asserting the one thing invariant 28 forbids about bytes it never
		// looked at, in the command the README calls the check that validates a
		// deployment against reality.
		fmt.Fprintf(w, "corpus diff GREEN for the types it rewrites: no canonical origin "+
			"reached the browser\n  in Tier 1, no page re-serialised, every page the "+
			"length it should be. %d origin(s)\n  did reach it in Tier 2 types, which "+
			"this run does not fail on — see the line above.\n", tier2)
	case green:
		fmt.Fprintln(w, "corpus diff GREEN: no canonical origin reached the browser, "+
			"no page re-serialised, every page the length it should be")
	default:
		fmt.Fprintln(w, "corpus diff RED")
	}
	return green
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
