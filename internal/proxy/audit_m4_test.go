package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// TestJSONValidatorsDroppedOnEqualLengthRewrite is the same class of bug as
// M1's: length used as a proxy for "unchanged".
//
// Two hosts of equal length rewrite the body without changing its size, so the
// upstream's ETag and Last-Modified were forwarded describing content the
// upstream never sent. The browser's next revalidation gets a 304 and serves
// what it cached under that validator — the canonical-bearing body, on the
// variant host, indefinitely.
func TestJSONValidatorsDroppedOnEqualLengthRewrite(t *testing.T) {
	// "aaa.example" and "bbb.example" are the same length, which is what makes
	// the bug reachable; the fleet has equal-length host pairs.
	m, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse("https://aaa.example"),
		Variant:   origin.MustParse("https://bbb.example"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"link":"https:\/\/aaa.example\/x"}`
	h := newHarness(t, m, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", "Wed, 27 Aug 2026 00:00:00 GMT")
		io.WriteString(w, body)
	})

	resp, got := h.get(t, "bbb.example", "/wp-json/wp/v2/posts")
	if strings.Contains(string(got), "aaa.example") {
		t.Fatalf("body was not rewritten, so this test proves nothing: %s", got)
	}
	if e := resp.Header.Get("ETag"); e != "" {
		t.Errorf("ETag %s forwarded for a body the upstream never sent", e)
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		t.Errorf("Last-Modified %s forwarded for a body the upstream never sent", lm)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length %s forwarded", cl)
	}
}

// TestJSONClosesTheUpstreamBody is not visible over HTTP/1, where the body is
// read to EOF and the connection recycles anyway. Over HTTP/2, or on a short
// read, it is a leaked stream.
//
// ReverseProxy closes only what finishBody leaves in resp.Body, and the JSON
// case wrapped the rewritten bytes in a NopCloser — dropping the upstream body
// on the floor. The over-cap branch beside it already did this correctly.
func TestJSONClosesTheUpstreamBody(t *testing.T) {
	m := herrforsMap(t)
	p := &Proxy{Map: m, Stats: rewrite.NewStats(false), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	for _, ct := range []string{"application/json", "text/html"} {
		var closes atomic.Int32
		resp := &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": {ct}},
			Body:       countingCloser{io.NopCloser(strings.NewReader(`{"link":"https://www.herrfors.fi/x"}`)), &closes},
		}
		site, _ := m.SiteForHost(variantHost)
		st := &state{site: site, url: variant + "/wp-json/"}
		if err := p.finishBody(resp, st, false); err != nil {
			t.Fatal(err)
		}
		// What ReverseProxy does with whatever finishBody left behind.
		if _, err := io.ReadAll(resp.Body); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		if n := closes.Load(); n != 1 {
			t.Errorf("%s: upstream body Close() called %d times, want 1", ct, n)
		}
	}
}

type countingCloser struct {
	io.ReadCloser
	n *atomic.Int32
}

func (c countingCloser) Close() error {
	c.n.Add(1)
	return c.ReadCloser.Close()
}

// TestTextJSONIsRewritten: text/json is not a registered media type, but it is
// what several WordPress plugins send, and bodyKind already treats text/* as
// rewritable on the *request* side — so the two directions disagreed about the
// same body, and a response leaked.
func TestTextJSONIsRewritten(t *testing.T) {
	m := herrforsMap(t)
	h := newHarness(t, m, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/json")
		io.WriteString(w, `{"link":"https://www.herrfors.fi/x"}`)
	})
	_, got := h.get(t, variantHost, "/wp-json/")
	if strings.Contains(string(got), "www.herrfors.fi") {
		t.Errorf("text/json left unrewritten: %s", got)
	}
}
