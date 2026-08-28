// Package corpus implements the corpus diff — PLAN §7's "only test that
// validates against reality".
//
// Fixtures would not have caught the double-port bug. This would: it crawls N
// URLs on the canonical site and the same N through the proxy, rewrites the
// canonical bytes through the same engine the proxy uses, and compares.
package corpus

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
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

	// The canonical bytes through the same engine the proxy runs.
	want, err := io.ReadAll(rewrite.NewResponseBody(
		strings.NewReader(string(canon)), o.Map.Forward(), nil, rewrite.Options{}))
	if err != nil {
		r.Err = err
		return r
	}

	r.Equal = string(want) == string(variant)
	r.LinesCanonical = strings.Count(string(want), "\n")
	r.LinesVariant = strings.Count(string(variant), "\n")
	r.DiffLines = countDiffLines(string(want), string(variant))

	// The safety-critical assertion, independent of byte equality: a live site
	// differs between two fetches for a dozen innocent reasons (nonces,
	// timestamps, ad slots), but a canonical origin reaching the browser is
	// never innocent.
	for _, o := range o.Map.Sites {
		for _, c := range o.CanonicalSet() {
			r.Leaks += strings.Count(string(variant), "//"+c.Host+"/")
			r.Leaks += strings.Count(string(variant), `\/\/`+c.Host+`\/`)
		}
	}
	return r
}

func fetch(ctx context.Context, o Options, base *url.URL, path string) ([]byte, error) {
	u := *base
	ref, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	u.Path, u.RawQuery = ref.Path, ref.RawQuery

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	defer res.Body.Close()
	return io.ReadAll(res.Body)
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

		body, err := fetch(ctx, o, o.Canonical, p)
		if err != nil {
			continue
		}
		for _, link := range links(body) {
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
	var equal, leaks, errs int

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
