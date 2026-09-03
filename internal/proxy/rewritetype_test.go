package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// --rewrite-type is the operator override for §5.2's Tier 2 exclusion. The
// exclusion is a measured default, not an oversight, so the first thing to pin
// is that the default did not move: naming a type must be the only way CSS and
// JavaScript are ever rewritten.
//
// The second is that it moves *both* directions together. `bodyKind`'s `text/`
// prefix already claimed text/css on the request side while the response side
// excluded it, so before this flag existed the two arms disagreed about the same
// bytes — the failure this file's comments have declared closed twice. A type
// rewritten outbound and not inbound is a variant hostname reaching the shared
// database, which is §4.3 and has no undo.

func TestTierTwoTypesAreExcludedUntilNamed(t *testing.T) {
	css := "text/css"
	js := "application/javascript"
	tjs := "text/javascript"

	for _, mt := range []string{css, js, tjs} {
		if rewritableText(mt, nil) {
			t.Errorf("%s is rewritten by default — §5.2 excludes it, and every "+
				"response of that type would now be buffered to the size cap "+
				"instead of streamed", mt)
		}
	}

	// Named, and only the named one moves.
	only, err := ParseTypes([]string{css})
	if err != nil {
		t.Fatal(err)
	}
	if !rewritableText(css, only) {
		t.Error("--rewrite-type text/css did not put text/css in the set")
	}
	for _, mt := range []string{js, tjs} {
		if rewritableText(mt, only) {
			t.Errorf("naming text/css also enabled %s — the flag has to name "+
				"exactly what it names", mt)
		}
	}
}

func TestTierThreeIsOnByDefault(t *testing.T) {
	// Feeds and sitemaps. PLAN §5.2's Tier 3 line says "drop for now"; the code
	// promoted them and the prose did not follow. This is the assertion that
	// says which one is true.
	for _, mt := range []string{
		"application/rss+xml", "application/atom+xml",
		"text/xml", "application/xml", "image/svg+xml",
		"application/vnd.sitemap+xml",
	} {
		if !rewritableText(mt, nil) {
			t.Errorf("%s is not rewritten by default — a feed and a sitemap are "+
				"full of absolute URLs and a reader dereferences them", mt)
		}
	}
}

func TestRewriteTypeMovesBothDirectionsTogether(t *testing.T) {
	for _, mt := range []string{"text/css", "application/javascript", "text/javascript"} {
		set, err := ParseTypes([]string{mt})
		if err != nil {
			t.Fatal(err)
		}
		out := rewritableText(mt, set)
		in := bodyKind(mt, set) == bodyFlat
		if out != in {
			t.Errorf("%s: response arm rewrites=%v, request arm rewrites=%v — a "+
				"body mapped one way and not the other reaches the shared "+
				"database in variant space (§4.3)", mt, out, in)
		}
	}
}

func TestParseTypesRefusesWhatWouldSilentlyNeverMatch(t *testing.T) {
	for _, bad := range []string{
		"text/css; charset=utf-8", // the gate compares a bare media type
		"css",                     // not type/subtype
		"text/",
		"/css",
	} {
		if _, err := ParseTypes([]string{bad}); err == nil {
			t.Errorf("ParseTypes(%q) was accepted — it would never match a "+
				"response and the operator would think the flag was on", bad)
		}
	}
	// Empty is no value, as every other repeatable flag here treats it: a
	// compose file cannot leave a flag out conditionally.
	if s, err := ParseTypes([]string{}); err != nil || s != nil {
		t.Errorf("ParseTypes(nil) = %v, %v; want nil, nil", s, err)
	}
	// Case and surrounding space are the operator's, not the gate's.
	s, err := ParseTypes([]string{" TEXT/CSS "})
	if err != nil {
		t.Fatal(err)
	}
	if !rewritableText("text/css; charset=utf-8", s) {
		t.Error("a type given in caps did not match the response that carries it")
	}
}

// And the whole proxy, because a predicate agreeing with itself is not the
// property — the bytes reaching the browser are.
func TestProxyRewritesCSSOnlyWhenTheTypeIsNamed(t *testing.T) {
	const canon = "https://www.example.fi"
	const variant = "https://wt-a--example.ddev.site"
	body := `@font-face{src:url(https://www.example.fi/app/fonts/x.woff2)}`

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		io.WriteString(w, body)
	}))
	defer up.Close()

	run := func(extra TypeSet) string {
		u, _ := url.Parse(up.URL)
		m, err := origin.NewMap([]origin.Site{{
			Name:      "acme",
			Canonical: origin.MustParse(canon),
			Variant:   origin.MustParse(variant),
		}})
		if err != nil {
			t.Fatal(err)
		}
		p := &Proxy{Upstream: u, Map: m, Stats: rewrite.NewStats(false),
			RewriteTypes: extra}
		srv := httptest.NewServer(p.Handler())
		defer srv.Close()

		req, _ := http.NewRequest("GET", srv.URL+"/x.css", nil)
		req.Host = "wt-a--example.ddev.site"
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	if got := run(nil); !strings.Contains(got, "www.example.fi") {
		t.Errorf("text/css was rewritten with no --rewrite-type — §5.2 excludes "+
			"it by default\n got: %s", got)
	}
	set, err := ParseTypes([]string{"text/css"})
	if err != nil {
		t.Fatal(err)
	}
	got := run(set)
	if strings.Contains(got, "www.example.fi") {
		t.Errorf("--rewrite-type text/css left a production origin in a "+
			"stylesheet the browser fetches — test 28\n got: %s", got)
	}
	if !strings.Contains(got, "wt-a--example.ddev.site") {
		t.Errorf("--rewrite-type text/css produced neither origin\n got: %s", got)
	}
}
