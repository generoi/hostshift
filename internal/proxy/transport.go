package proxy

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// decodeBody prepares a response body for rewriting (PLAN §5.2, Compression).
//
// hostshift asks for identity upstream, so the common path needs no decoder at
// all. gzip is kept as a fallback for upstreams that compress regardless.
// Anything else is passed through byte-identical and logged as skipped — never
// rewrite what cannot be decoded (test 26). deflate is not worth implementing
// and bzip2 is not an HTTP content encoding.
//
// It reports whether the body may be rewritten.
func (p *Proxy) decodeBody(resp *http.Response) bool {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	switch enc {
	case "", "identity":
		return true

	case "gzip", "x-gzip":
		// gzip.NewReader consumes the header before it can fail, so the head is
		// captured first and put back if it turns out not to be gzip at all.
		// Without this, a mislabelled body loses however many bytes the header
		// check read — which is the whole body when it is short.
		head := make([]byte, 512)
		n, _ := io.ReadFull(resp.Body, head)
		head = head[:n]
		rest := io.MultiReader(bytes.NewReader(head), resp.Body)

		zr, err := gzip.NewReader(rest)
		if err != nil {
			// Labelled gzip but not gzip. Passing it through is the honest
			// answer; the body was never ours to reinterpret.
			p.log().Warn("body is labelled gzip but does not decode; passing it through", "err", err)
			p.skipEncoding(enc)
			resp.Body = readCloser{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
			return false
		}
		// Decoded downstream by default: identity on the browser side is what
		// keeps behaviour under test the same as behaviour in a browser, and
		// over loopback compression buys nothing.
		resp.Body = gzipBody{zr, resp.Body}
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		return true

	default:
		// br, zstd, deflate, or anything else.
		p.log().Info("content-encoding cannot be decoded; body passed through byte-identical", "encoding", enc)
		p.skipEncoding(enc)
		return false
	}
}

func (p *Proxy) skipEncoding(enc string) {
	p.Stats.Record(rewrite.SurfaceHeader, 0, []origin.Event{{
		Surface: rewrite.SurfaceHeader, Text: enc,
		Action: origin.ActionSkipped, Reason: origin.ReasonNotDecodable,
	}})
}

// gzipBody closes both the decompressor and the underlying body.
type gzipBody struct {
	*gzip.Reader
	under io.Closer
}

func (g gzipBody) Close() error {
	err := g.Reader.Close()
	if cerr := g.under.Close(); err == nil {
		err = cerr
	}
	return err
}

// compressBody re-encodes a rewritten body per the client's Accept-Encoding.
//
// Off by default. It exists for performance work, where transfer size and
// Content-Encoding must resemble production. v0.2's mistake was the opposite —
// forcing identity on the *browser* side unconditionally, which silently changed
// behaviour under test.
func (p *Proxy) compressBody(resp *http.Response, accept string) {
	if !acceptsGzip(accept) {
		return
	}
	// A status that forbids a body, or a HEAD. Gzipping nothing yields the
	// 23-byte empty stream, and announcing it is a lie in both directions:
	// net/http refuses to write a body on a 204 and drops the connection
	// instead, so every DELETE /wp-json/…, Heartbeat and autosave became a
	// network error with no HTTP status to diagnose; and a HEAD is required to
	// report the length a GET would send, so anything sizing a resource before
	// fetching it — wp_remote_head, a link checker, an uptime probe — was told
	// 23 bytes and then handed the real body.
	if !bodyAllowedForStatus(resp.StatusCode) || resp.Request != nil && resp.Request.Method == http.MethodHead {
		return
	}
	// An identity map must be a no-op end to end, not merely in the body
	// (test 24). Re-encoding a response nothing rewrote is pure perturbation,
	// and leaving it ungated meant the guard rail could not be run in the
	// --compress configuration at all.
	if p.Map.Identity() {
		return
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(nil))
		return
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(body); err != nil || zw.Close() != nil {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return
	}
	resp.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
	resp.Header.Set("Content-Encoding", "gzip")
	resp.Header.Set("Content-Length", itoa(buf.Len()))
	resp.ContentLength = int64(buf.Len())
	addVary(resp.Header, "Accept-Encoding")
}

// bodyAllowedForStatus mirrors net/http's own rule, which is what decides
// whether the server will write what compressBody produces.
func bodyAllowedForStatus(status int) bool {
	switch {
	case status >= 100 && status <= 199:
		return false
	case status == http.StatusNoContent, status == http.StatusNotModified:
		return false
	}
	return true
}

// acceptsGzip reports whether the client will take gzip, honouring q-values and
// the wildcard.
//
// The q-value has to be parsed rather than matched as a substring: "q=0.9"
// contains "q=0", so every client that weights its preferences — the
// "br;q=1.0, gzip;q=0.8" shape curl and several HTTP libraries send — was read
// as refusing gzip outright, and --compress silently did nothing. The flag
// exists only to make transfer size resemble production for performance work,
// so a silent no-op means the measurement it was built for reports the wrong
// number.
func acceptsGzip(accept string) bool {
	wildcard := false
	for _, part := range strings.Split(accept, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		name = strings.TrimSpace(name)
		switch {
		case strings.EqualFold(name, "gzip"):
			return qValue(params) > 0
		case name == "*":
			wildcard = qValue(params) > 0
		}
	}
	return wildcard
}

// qValue reads the q= parameter of one Accept-Encoding element. Absent means 1.
func qValue(params string) float64 {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(p, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 1 // unparseable: treat it as unweighted rather than as a refusal
		}
		return q
	}
	return 1
}

// addVary appends a field to Vary without duplicating it, so that a shared cache
// downstream cannot serve one client's body to another (PLAN §5.5).
func addVary(h http.Header, field string) {
	for _, v := range h.Values("Vary") {
		for _, f := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(f), field) {
				return
			}
			if strings.TrimSpace(f) == "*" {
				return
			}
		}
	}
	h.Add("Vary", field)
}

// isPartial reports a response that must not be rewritten.
//
// Rewriting a partial body is incoherent: the origin the client asked for may be
// split across range boundaries, and the byte offsets the client is assembling
// would no longer line up. Range responses are a stated non-goal (§6); they are
// passed through and logged rather than mangled.
//
// Which is only safe because Range never reaches the upstream — see
// stripRange. It used to, and a 206 skips every rewriter, so any client could
// turn the whole engine off by asking for "bytes=0-<len-1>" and get the entire
// document back with every production origin intact. That is test 28, and
// unlike the self-redirect carve-out — which §7 enumerates as exactly one
// header — it was selectable by whoever was browsing.
func isPartial(resp *http.Response) bool {
	return resp.StatusCode == http.StatusPartialContent ||
		resp.Header.Get("Content-Range") != ""
}

// stripRange removes range conditionals from an outbound request.
//
// §5.5 says "Range requests — rewriting a partial body is incoherent; bypass".
// The only reading of that which is safe is bypassing the *Range*, not the
// rewriter: whether a response is rewritable is not knowable until it comes
// back, and by then a 206 cannot be turned into something that can be rewritten
// without a second round trip. nginx does not serve ranges for a PHP response,
// so in the fleet this costs nothing on a page; it costs a media file being
// re-fetched from zero on a seek, over loopback or the Docker bridge.
//
// RFC 9110 lets a server ignore Range and answer 200, which is what a client
// gets here. Accept-Ranges is dropped from *rewritten* responses only, along
// with the other validators — an untouched body keeps it, so a client may still
// ask for a range on a media file and be answered in full.
func stripRange(h http.Header) {
	h.Del("Range")
	h.Del("If-Range")
}
