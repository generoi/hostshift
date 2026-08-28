package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// BenchmarkE2EPage measures a whole request, which is the only number that
// tracks what a browser waits for.
//
// Its absence is why the flush bug survived M6 and a performance pass. Every
// benchmark measured the rewriter in isolation, where io.Copy hands it a 32 KiB
// buffer and there is no HTTP layer — so the shape that cost 3x end to end
// looked like a 3.9% *win* in the microbenchmark, and was written into
// docs/performance.md as a thing not to change.
//
// Keep this one honest and the microbenchmarks can lie all they like.
func BenchmarkE2EPage(b *testing.B) {
	body, err := os.ReadFile("../../spike/corpus/page1.html")
	if err != nil {
		b.Skip(err)
	}
	m, err := origin.NewMap([]origin.Site{{
		Name: "main", Canonical: origin.MustParse("https://www.herrfors.fi"),
		Variant: origin.MustParse("https://wt-a--herrfors.ddev.site"),
	}})
	if err != nil {
		b.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(body)
	}))
	defer up.Close()
	target, _ := url.Parse(up.URL)
	p := &Proxy{Upstream: target, Map: m, Stats: rewrite.NewStats(false), Log: discardLogger()}
	front := httptest.NewServer(p.Handler())
	defer front.Close()

	c := &http.Client{}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req, _ := http.NewRequest("GET", front.URL+"/", nil)
		req.Host = "wt-a--herrfors.ddev.site"
		res, err := c.Do(req)
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
}
