package proxy

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// TestGzipUpstreamDecoded is the first half of acceptance test 9. hostshift asks
// for identity upstream, but an upstream that compresses regardless must still
// be rewritten rather than passed through with canonical origins inside.
func TestGzipUpstreamDecoded(t *testing.T) {
	page := `<a href="` + canonical + `/x">t</a>`
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(page))
	zw.Close()

	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(buf.Bytes())
	})
	res, got := h.get(t, variantHost, "/")

	if !bytes.Contains(got, []byte(variant+"/x")) {
		t.Errorf("a gzip body was not decoded and rewritten: %q", got)
	}
	if res.Header.Get("Content-Encoding") != "" {
		t.Errorf("Content-Encoding survived on a decoded body: %q", res.Header.Get("Content-Encoding"))
	}
}

// TestUndecodableEncodingPassedThrough is acceptance test 26 and the second half
// of test 9: never rewrite what cannot be decoded.
func TestUndecodableEncodingPassedThrough(t *testing.T) {
	// Bytes that spell a canonical origin, labelled with an encoding hostshift
	// has no decoder for. Rewriting them would corrupt the stream.
	body := []byte("\x1b[brotli]" + canonical + "/x")

	for _, enc := range []string{"br", "zstd", "deflate", "compress"} {
		h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Encoding", enc)
			w.Write(body)
		})
		_, got := h.get(t, variantHost, "/")
		if !bytes.Equal(got, body) {
			t.Errorf("%s: body was not byte-identical (%d in, %d out)", enc, len(body), len(got))
		}
		if n := h.stats.Snapshot().Skips[origin.ReasonNotDecodable]; n != 1 {
			t.Errorf("%s: the skip was not logged as a counter (%d)", enc, n)
		}
	}
}

// TestGzipLabelledButNotGzip: a body that lies about its encoding is passed
// through whole rather than half-read.
//
// gzip.NewReader consumes the header before it can fail, so without the head
// being captured and put back, a short mislabelled body loses every byte the
// header check read. The request asks for identity so that Go's own transport
// does not try to decode the response as well.
func TestGzipLabelledButNotGzip(t *testing.T) {
	body := []byte("this is not gzip " + canonical + "/x")
	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(body)
	})

	req, _ := http.NewRequest("GET", h.front.URL+"/", nil)
	req.Host = variantHost
	req.Header.Set("Accept-Encoding", "identity")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("a mislabelled body was modified:\n got %q\nwant %q", got, body)
	}
	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Error("the encoding label was changed; hostshift must not relabel a body it did not decode")
	}
}

// TestIdentityDownstreamByDefault: the browser gets an uncompressed body unless
// --compress is set. v0.2's mistake was the opposite — forcing identity on the
// browser side unconditionally, silently changing behaviour under test.
func TestIdentityDownstreamByDefault(t *testing.T) {
	page := `<a href="` + canonical + `/x">t</a>`
	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, page)
	})
	req, _ := http.NewRequest("GET", h.front.URL+"/", nil)
	req.Host = variantHost
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if got := res.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding is %q; identity downstream is the default", got)
	}
}

// TestCompressOptIn: --compress re-encodes per the client's Accept-Encoding, and
// says so in Vary.
func TestCompressOptIn(t *testing.T) {
	page := `<a href="` + canonical + `/x">t</a>`
	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, page)
	}, func(p *Proxy) { p.Compress = true })

	// A client that wants gzip gets it, and the body still decodes to the
	// rewritten page.
	req, _ := http.NewRequest("GET", h.front.URL+"/", nil)
	req.Host = variantHost
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding is %q, want gzip", got)
	}
	zr, err := gzip.NewReader(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	res.Body.Close()
	if !bytes.Contains(got, []byte(variant+"/x")) {
		t.Errorf("the compressed body is not the rewritten page: %q", got)
	}
	if !hasVary(res.Header, "Accept-Encoding") {
		t.Errorf("Vary does not mention Accept-Encoding: %q", res.Header.Values("Vary"))
	}

	// A client that does not want it is not given it.
	req2, _ := http.NewRequest("GET", h.front.URL+"/", nil)
	req2.Host = variantHost
	req2.Header.Set("Accept-Encoding", "identity")
	res2, err := http.DefaultTransport.RoundTrip(req2)
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if got := res2.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding is %q for a client asking for identity", got)
	}
}

// TestVaryOnHost: the body depends on the request Host, so a shared cache
// downstream must not serve one variant's body to another (PLAN §5.5).
func TestVaryOnHost(t *testing.T) {
	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Vary", "Accept-Language")
		io.WriteString(w, `<a href="`+canonical+`/x">t</a>`)
	})
	res, _ := h.get(t, variantHost, "/")
	if !hasVary(res.Header, "Host") {
		t.Errorf("Vary does not mention Host: %q", res.Header.Values("Vary"))
	}
	if !hasVary(res.Header, "Accept-Language") {
		t.Errorf("the upstream's own Vary was dropped: %q", res.Header.Values("Vary"))
	}
}

func hasVary(h http.Header, field string) bool {
	for _, v := range h.Values("Vary") {
		for _, f := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(f), field) {
				return true
			}
		}
	}
	return false
}

// TestRangeResponsePassedThrough: rewriting a partial body is incoherent — the
// client is assembling byte offsets that a length-changing rewrite would break.
// Range responses are a stated non-goal (§6).
func TestRangeResponsePassedThrough(t *testing.T) {
	part := []byte(`href="` + canonical + `/x"`)
	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Range", "bytes 10-32/1000")
		w.WriteHeader(http.StatusPartialContent)
		w.Write(part)
	})
	res, got := h.get(t, variantHost, "/big.html")
	if res.StatusCode != http.StatusPartialContent {
		t.Errorf("status %d, want 206", res.StatusCode)
	}
	if !bytes.Equal(got, part) {
		t.Errorf("a 206 body was rewritten:\n got %q\nwant %q", got, part)
	}
}

// TestStreamingBoundedByLargestToken is acceptance test 13: a large streamed
// HTML response buffers at most one token, so peak memory is O(largest token)
// rather than O(response size).
func TestStreamingBoundedByLargestToken(t *testing.T) {
	// ~5 MB of small tokens.
	var page bytes.Buffer
	page.WriteString("<html><body>")
	row := `<a href="https://c.example/x" class="row">cell</a>` + "\n"
	for page.Len() < 5<<20 {
		page.WriteString(row)
	}
	page.WriteString("</body></html>")

	m, err := origin.NewMatcher([]origin.Pair{{
		Name: "main", Canonical: origin.MustParse("https://c.example"), Variant: origin.MustParse("https://v.example"),
	}})
	if err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	rw := rewrite.NewHTML(bytes.NewReader(page.Bytes()), m, nil, rewrite.Options{})
	var peakHeap uint64
	buf := make([]byte, 4096)
	n := 0
	for {
		c, err := rw.Read(buf)
		n += c
		if c > 0 && n%(512<<10) < 4096 {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			if d := ms.HeapAlloc; d > peakHeap {
				peakHeap = d
			}
		}
		if err != nil {
			break
		}
	}

	// The direct assertion: the token buffer never held more than one token's
	// worth. A response-sized buffer would be ~5 MB.
	if got := rw.MaxBuffered(); got > 64<<10 {
		t.Errorf("the rewriter buffered %d bytes of a %d-byte response; it must hold at most one token",
			got, page.Len())
	}
	t.Logf("5 MB response, %d bytes out, max token buffer %d bytes, peak heap %d bytes",
		n, rw.MaxBuffered(), peakHeap)
}

// TestOversizeTokenPassesThroughRatherThanAborting: a single token larger than
// the cap yields ErrBufferExceeded mid-body, which per §5.7 cannot become an
// error response once headers are sent. The remainder streams through unparsed,
// and §4.4's sweep — which sits downstream — still catches the origins in it.
func TestOversizeTokenPassesThroughRatherThanAborting(t *testing.T) {
	big := `<script>var a="` + strings.Repeat("x", 200<<10) + `https://c.example/x";</script>`
	page := "<p>before</p>" + big + `<a href="https://c.example/after">after</a>`

	m, err := origin.NewMatcher([]origin.Pair{{
		Name: "main", Canonical: origin.MustParse("https://c.example"), Variant: origin.MustParse("https://v.example"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	st := rewrite.NewStats(false)
	out, err := io.ReadAll(rewrite.NewResponseBody(strings.NewReader(page), m, nil, rewrite.Options{
		MaxToken: 4096, // far below the script token
		Stats:    st,
		Log:      discardLogger(),
	}))
	if err != nil {
		t.Fatalf("an oversize token aborted the response: %v", err)
	}
	if len(out) != len(page) {
		t.Errorf("output is %d bytes, input %d — the tail was truncated", len(out), len(page))
	}
	// The sweep sits downstream of the rewriter, so nothing leaks even though
	// the tail was never parsed.
	if bytes.Contains(out, []byte("c.example")) {
		t.Errorf("a canonical origin survived the passthrough tail")
	}
	if st.Snapshot().Skips[origin.ReasonSizeCap] == 0 {
		t.Error("the size-cap skip was not counted")
	}
}
