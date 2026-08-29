package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// TestXPingbackIsRewritten. WordPress core sends X-Pingback: <site_url>/xmlrpc.php
// on every singular view, and originHeaders is a closed allowlist that did not
// name it — so a dereferenceable production origin reached the browser on most
// pages, pointing at a *write* endpoint, which is the hazard §4.4 weighs the
// self-redirect carve-out against. Test 28's Go implementation only ever scanned
// the HTML body, and the shell suite checks headers by name, so neither could
// see it.
func TestXPingbackIsRewritten(t *testing.T) {
	const canon, vari = "https://www.example.fi", "https://wt-a--ex.ddev.site"
	m, err := origin.NewMap([]origin.Site{{
		Name: "main", Canonical: origin.MustParse(canon), Variant: origin.MustParse(vari),
	}})
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Pingback", canon+"/xmlrpc.php")
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<p>x</p>"))
	}))
	defer up.Close()
	target, _ := url.Parse(up.URL)
	p := &Proxy{Upstream: target, Map: m, Stats: rewrite.NewStats(false)}
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	req, _ := http.NewRequest("GET", front.URL+"/post", nil)
	req.Host = "wt-a--ex.ddev.site"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	got := res.Header.Get("X-Pingback")
	if strings.Contains(got, "www.example.fi") {
		t.Errorf("X-Pingback reached the browser naming production: %q", got)
	}
	if !strings.Contains(got, "wt-a--ex.ddev.site") {
		t.Errorf("X-Pingback = %q, want the variant host", got)
	}
}

// TestTextJSONRequestUsesTheJSONPath. The response side already treats text/json
// as JSON; the request side matched the "text/" prefix first and took the flat
// path, which rewrites JSON *keys*, gives --explain no RFC 6901 path, and
// half-rewrites a malformed body. The two directions disagreed about the same
// body — the thing rewritableJSON's own comment says text/json was added to
// prevent.
func TestTextJSONRequestUsesTheJSONPath(t *testing.T) {
	for _, ct := range []string{"text/json", "application/json", "application/ld+json"} {
		if got := bodyKind(ct); got != bodyJSON {
			t.Errorf("bodyKind(%q) = %v, want bodyJSON", ct, got)
		}
	}
	if got := bodyKind("text/plain"); got != bodyFlat {
		t.Errorf("bodyKind(text/plain) = %v, want bodyFlat", got)
	}
}
