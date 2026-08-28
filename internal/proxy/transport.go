package proxy

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
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

func acceptsGzip(accept string) bool {
	for _, part := range strings.Split(accept, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		// q=0 means "explicitly not this one".
		return !strings.Contains(strings.ReplaceAll(params, " ", ""), "q=0")
	}
	return false
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
func isPartial(resp *http.Response) bool {
	return resp.StatusCode == http.StatusPartialContent ||
		resp.Header.Get("Content-Range") != ""
}
