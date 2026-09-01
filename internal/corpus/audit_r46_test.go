package corpus

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
)

// Round 46. 598de7c put pages ahead of subresources in the crawl. Probe: on a
// site with more linked pages than the budget, is a stylesheet ever fetched?
func TestR46SubresourcesStarveOnARealSite(t *testing.T) {
	// A homepage with one stylesheet and 40 links, and every linked page links
	// 40 more — an ordinary WordPress menu.
	page := func(prefix string) string {
		var b strings.Builder
		b.WriteString(`<html><head><link rel="stylesheet" href="/style.css"></head><body>`)
		for i := 0; i < 40; i++ {
			fmt.Fprintf(&b, `<a href="/%s-%d/">l</a>`, prefix, i)
		}
		b.WriteString(`</body></html>`)
		return b.String()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".css") {
			w.Header().Set("Content-Type", "text/css")
			_, _ = w.Write([]byte("body{}"))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page(strings.Trim(r.URL.Path, "/") + "x")))
	}))
	defer srv.Close()

	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	m, err := origin.NewMap([]origin.Site{{
		Name:      "r46",
		Canonical: origin.MustParse(srv.URL),
		Variant:   origin.MustParse(srv.URL),
	}})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := crawl(context.Background(), Options{
		Canonical: base, Variant: base, Map: m, N: 20, Client: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	css := 0
	for _, p := range paths {
		if strings.HasSuffix(p, ".css") {
			css++
		}
	}
	t.Logf("crawled %d paths, %d of them css:\n  %s", len(paths), css, strings.Join(paths, "\n  "))
	if css == 0 {
		t.Errorf("no subresource was fetched at all, so Result.Tier2 — the count "+
			"links() exists to make reachable — is structurally zero on any site with "+
			"more linked pages than -n. paths=%v", paths)
	}
}
