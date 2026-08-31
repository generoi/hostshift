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

	// Leaks counts canonical origins in the variant response. Any non-zero
	// value is a test 28 failure and is what this whole exercise is for.
	Leaks int
	// LinesCanonical and LinesVariant should match even when bytes do not:
	// splicing never rebuilds whitespace, so a line-count change means
	// something re-serialised.
	LinesCanonical, LinesVariant int
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
		unchangedSelfRedirect := !o.StrictOrigins &&
			variant.location == canon.location && canon.location != ""
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

	for len(queue) > 0 && (o.N == 0 || len(out) < o.N) {
		p := queue[0]
		queue = queue[1:]
		out = append(out, p)

		res, err := fetch(ctx, o, o.Canonical, p)
		if err != nil {
			continue
		}
		for _, link := range links(res.body) {
			u, err := url.Parse(link)
			if err != nil || u.Path == "" {
				continue
			}
			// Same site only: a crawl that wanders onto a third-party host is
			// measuring nothing.
			if u.Host != "" && !sameSite[strings.ToLower(u.Hostname())] {
				continue
			}
			if u.Fragment != "" && u.Path == p {
				continue
			}
			key := u.Path
			if u.RawQuery != "" {
				key += "?" + u.RawQuery
			}
			if !seen[key] {
				seen[key] = true
				queue = append(queue, key)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func links(body []byte) []string {
	var out []string
	z := html.NewTokenizer(strings.NewReader(string(body)))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return out
		}
		if tt != html.StartTagToken {
			continue
		}
		name, hasAttr := z.TagName()
		if string(name) != "a" {
			continue
		}
		for hasAttr {
			var k, v []byte
			k, v, hasAttr = z.TagAttr()
			if string(k) == "href" {
				out = append(out, string(v))
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
// canonical origin reached the browser, and no page lost or gained lines.
func WriteReport(w io.Writer, results []Result) bool {
	green := true
	var equal, leaks, errs, tier2 int

	fmt.Fprintf(w, "%-46s %-8s %-7s %-7s %s\n", "PATH", "BYTES", "LEAKS", "LINES", "NOTE")
	for _, r := range results {
		note := ""
		switch {
		case r.Err != nil:
			note, errs = r.Err.Error(), errs+1
			green = false
		case r.Leaks > 0:
			note = "CANONICAL ORIGIN REACHED THE BROWSER"
			leaks += r.Leaks
			green = false
		case r.Tier2 > 0:
			// Not a defect: PLAN's fast path excludes these types "per Tier 2,
			// and added only if the corpus diff shows a leak". This is that
			// showing. It does not turn the run RED, because the proxy is doing
			// what it says it does — but it is the trigger, so it is loud.
			note = fmt.Sprintf("%d origins in a Tier 2 type (%s) — the PLAN's "+
				"trigger for rewriting it", r.Tier2, r.ContentType)
			tier2 += r.Tier2
		case r.LinesCanonical != r.LinesVariant:
			note = "line count changed — something re-serialised"
			green = false
		case r.Equal:
			equal++
		default:
			note = fmt.Sprintf("%d lines differ (dynamic content?)", r.DiffLines)
		}
		state := "differ"
		if r.Equal {
			state = "same"
		}
		fmt.Fprintf(w, "%-46s %-8s %-7d %-7s %s\n",
			truncate(r.Path, 46), state, r.Leaks,
			fmt.Sprintf("%d/%d", r.LinesCanonical, r.LinesVariant), note)
	}

	fmt.Fprintf(w, "\n%d pages, %d byte-identical, %d leaks, %d errors\n",
		len(results), equal, leaks, errs)
	if tier2 > 0 {
		fmt.Fprintf(w, "%d origins in Tier 2 types (text/css, JavaScript), which the "+
			"proxy excludes by design — PLAN's fast path adds them \"only if the "+
			"corpus diff shows a leak\", and this is that\n", tier2)
	}
	if green {
		fmt.Fprintln(w, "corpus diff GREEN: no canonical origin reached the browser, no page re-serialised")
	} else {
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
