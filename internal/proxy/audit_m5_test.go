package proxy

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/generoi/hostshift/internal/origin"
)

// TestRangeCannotDisableRewriting is test 28 for a header any client can send.
//
// A 206 skips every rewriter, and Range used to be forwarded upstream verbatim
// — so "Range: bytes=0-<len-1>" returned the whole document with every
// production origin intact. §7 enumerates the self-redirect Location as the one
// carve-out; this was a second one, selectable by whoever was browsing.
func TestRangeCannotDisableRewriting(t *testing.T) {
	body := `<!doctype html><a href="` + canonical + `/secret">go</a>` + strings.Repeat("p", 200)
	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		if rng := r.Header.Get("Range"); rng != "" {
			t.Errorf("Range %q reached the upstream", rng)
		}
		w.Header().Set("Content-Type", "text/html")
		// http.ServeContent is what nginx and http.FileServer do for a static
		// .html, and it honours Range whenever one arrives.
		http.ServeContent(w, r, "x.html", time.Time{}, strings.NewReader(body))
	})

	req, err := http.NewRequest("GET", h.front.URL+"/x.html", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = variantHost
	req.Header.Set("Range", "bytes=0-"+itoa(len(body)-1))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusPartialContent {
		t.Errorf("a client-chosen Range produced a 206, which skips every rewriter")
	}
	if strings.Contains(string(got), "www.acmecorp.fi") {
		t.Errorf("test 28: a production origin reached the browser:\n%s", got)
	}
	if ar := resp.Header.Get("Accept-Ranges"); ar != "" {
		t.Errorf("Accept-Ranges %q invites the client to ask again", ar)
	}
}

// TestVaryOnEveryRewrittenResponse: addVary sat after the content-type switch,
// whose default arm returns early — so the responses that most need it never
// got it. A 302 whose Location had just been rewritten into variant space went
// out with no Vary, and a shared cache keyed on path alone hands variant A's
// redirect to a browser on variant B, bouncing it out of its own worktree.
func TestVaryOnEveryRewrittenResponse(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", canonical+"/dashboard")
			w.WriteHeader(http.StatusFound)
		})
		res, _ := h.get(t, variantHost, "/login")
		if !strings.Contains(res.Header.Get("Location"), variantHost) {
			t.Fatalf("Location was not rewritten, so this proves nothing: %q", res.Header.Get("Location"))
		}
		assertVaryHost(t, res)
	})

	t.Run("undecodable encoding", func(t *testing.T) {
		h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Encoding", "br")
			w.Header().Set("Link", "<"+canonical+"/wp-json/>; rel=\"x\"")
			w.Write([]byte("\x00\x01opaque"))
		})
		res, _ := h.get(t, variantHost, "/")
		if !strings.Contains(res.Header.Get("Link"), variantHost) {
			t.Fatalf("Link was not rewritten, so this proves nothing")
		}
		assertVaryHost(t, res)
	})

	t.Run("not a rewritable type", func(t *testing.T) {
		h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Link", "<"+canonical+"/wp-json/>; rel=\"x\"")
			w.Write([]byte("\x89PNG"))
		})
		res, _ := h.get(t, variantHost, "/a.png")
		assertVaryHost(t, res)
	})
}

func assertVaryHost(t *testing.T, res *http.Response) {
	t.Helper()
	for _, v := range res.Header.Values("Vary") {
		for _, f := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(f), "Host") {
				return
			}
		}
	}
	t.Errorf("a response rewritten into variant space carries no Vary: Host (got %v)",
		res.Header.Values("Vary"))
}

// TestDryRunDoesNotTouchTheResponse. §5.8 defines --dry-run as safe to point at
// a live canonical checkout, and its whole value is that it cannot perturb what
// it measures. It was gunzipping the body, stripping Content-Encoding and
// inventing a Vary header on a production origin.
func TestDryRunDoesNotTouchTheResponse(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(`<a href="` + canonical + `/x">t</a>`))
	zw.Close()
	raw := gz.Bytes()

	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(raw)
	}, func(p *Proxy) { p.DryRun = true; p.Log = discardLogger() })

	// An explicit Accept-Encoding, so Go's transport does not add its own and
	// transparently gunzip the response before the test can look at it.
	req, err := http.NewRequest("GET", h.front.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = variantHost
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(res.Body)

	if !bytes.Equal(got, raw) {
		t.Errorf("--dry-run changed the body: upstream sent %d bytes, browser got %d", len(raw), len(got))
	}
	if ce := res.Header.Get("Content-Encoding"); ce != "gzip" {
		t.Errorf("--dry-run stripped Content-Encoding (got %q)", ce)
	}
	if v := res.Header.Values("Vary"); len(v) != 0 {
		t.Errorf("--dry-run added Vary: %v", v)
	}
}

// TestCompressLeavesBodilessResponsesAlone. Gzipping nothing yields the 23-byte
// empty stream; announcing it on a 204 makes net/http refuse to write and drop
// the connection, so every DELETE /wp-json/…, Heartbeat and autosave became a
// network error with no status to diagnose. On a HEAD it reports 23 bytes for a
// resource a GET would return in full.
func TestCompressLeavesBodilessResponsesAlone(t *testing.T) {
	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/204" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		io.WriteString(w, `<a href="`+canonical+`/x">t</a>`)
	}, func(p *Proxy) { p.Compress = true; p.Log = discardLogger() })

	res, _ := h.do(t, "GET", variantHost, "/204", "", nil)
	if res.StatusCode != http.StatusNoContent {
		t.Errorf("204 came back as %d", res.StatusCode)
	}
	if ce := res.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("204 carries Content-Encoding %q", ce)
	}
	if cl := res.Header.Get("Content-Length"); cl != "" && cl != "0" {
		t.Errorf("204 claims Content-Length %q", cl)
	}

	head, _ := h.do(t, "HEAD", variantHost, "/", "", nil)
	if ce := head.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("HEAD carries Content-Encoding %q for a body it never sends", ce)
	}
	if cl := head.Header.Get("Content-Length"); cl == "23" {
		t.Error("HEAD reports the length of the empty gzip stream, not of what a GET would send")
	}
}

// TestAcceptsGzipHonoursQValues: "q=0.9" contains "q=0", so substring matching
// read every weighted client as refusing gzip and --compress silently did
// nothing — in the one mode whose entire purpose is measuring transfer size.
func TestAcceptsGzipHonoursQValues(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"gzip", true},
		{"gzip;q=0.9", true},
		{"gzip;q=0.5", true},
		{"br;q=1.0, gzip;q=0.8", true},
		{"gzip, deflate, br, zstd", true},
		{"*", true},
		{"*;q=0.5", true},
		{"gzip;q=0", false},
		{"gzip;q=0.0", false},
		{"*;q=0", false},
		{"br, zstd", false},
		{"", false},
	} {
		if got := acceptsGzip(c.in); got != c.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestCompressIsANoOpUnderAnIdentityMap is test 24 extended to the flag. An
// identity map has to be a no-op end to end, not merely in the body, or the
// guard rail cannot be run in this configuration at all.
func TestCompressIsANoOpUnderAnIdentityMap(t *testing.T) {
	body := `<a href="` + canonical + `/x">t</a>`
	idm, err := origin.NewMap([]origin.Site{{
		Name:      "main",
		Canonical: origin.MustParse(canonical),
		Variant:   origin.MustParse(canonical),
	}})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, idm, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, body)
	}, func(p *Proxy) { p.Compress = true; p.Log = discardLogger() })

	res, got := h.get(t, "www.acmecorp.fi", "/")
	if string(got) != body {
		t.Errorf("identity map changed the body:\n got %s\nwant %s", got, body)
	}
	if ce := res.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("identity map + --compress set Content-Encoding %q", ce)
	}
}

// TestProgressiveResponseIsNotHeld guards the reason response bodies are not
// batched.
//
// Batching reads is worth 3.1x end to end and is not taken: filling the caller's
// buffer means one more Read than there is data, which blocks, and that holds a
// progressively flushed response — wp-admin's update and import screens, which
// emit a few hundred bytes over a long operation — until 32 KiB has accumulated
// or the operation ends. See the note above readAhead's grave in proxy.go.
//
// The chunk is a kilobyte because §4.4's carry-over window sets a floor that has
// nothing to do with batching: the sweep holds back MaxMatchLen bytes so no
// match can straddle a boundary, so anything smaller is buffered whatever the
// reader above it does.
func TestProgressiveResponseIsNotHeld(t *testing.T) {
	step := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(step) }) }
	defer release() // never leave the upstream handler parked

	first := "<p>" + strings.Repeat("step one ", 120) + "</p>\n"
	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, first)
		w.(http.Flusher).Flush()
		<-step // hold the connection open, as a long operation would
		io.WriteString(w, "<p>step two</p>\n")
	})

	req, err := http.NewRequest("GET", h.front.URL+"/wp-admin/update-core.php", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = variantHost
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 32*1024)
		n, _ := res.Body.Read(buf)
		got <- string(buf[:n])
	}()

	select {
	case s := <-got:
		if !strings.Contains(s, "step one") {
			t.Errorf("first chunk was %q, want the first flush", s)
		}
	case <-time.After(5 * time.Second):
		t.Error("the first flush never reached the client: a progressive response is being held")
	}

	release()
	io.Copy(io.Discard, res.Body)
}
