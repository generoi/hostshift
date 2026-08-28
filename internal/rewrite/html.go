package rewrite

import (
	"bytes"
	"io"
	"strings"

	"golang.org/x/net/html"

	"github.com/generoi/hostshift/internal/origin"
)

// HTML rewrites origins in an HTML stream by locating spans and splicing, never
// re-serialising (PLAN §5.2).
//
// x/net/html's Tokenizer supplies the framing. Its Raw() carries a documented
// partition guarantee — consecutive tokens' raw bytes have no overlaps or gaps —
// so every untouched token is emitted verbatim and byte offsets come for free.
// html.Parse + html.Render is never used: the lossy round-trip reputation
// belongs entirely to the parser/renderer, which alphabetises attributes,
// lowercases names, decodes entities and inserts <tbody>.
//
// It is an io.Reader, so it streams: at most one token is buffered, never the
// whole body.
type HTML struct {
	z       *html.Tokenizer
	m       *origin.Matcher
	stats   *Stats
	dryRun  bool
	src     io.Closer
	rawText string
	pend    bytes.Buffer
	done    bool
	err     error // a non-EOF tokenizer error, surfaced once pending bytes are out
	inOff   int   // cumulative input-stream offset, for --explain
}

// Options configures a rewriter.
type Options struct {
	// DryRun computes and counts every rewrite but emits the input unchanged
	// (PLAN §5.8) — safe to point at a live canonical checkout.
	DryRun bool
	Stats  *Stats
}

// NewHTML wraps r. If src is non-nil, Close closes it — the proxy passes the
// upstream body so the rewriter can stand in for it as a ReadCloser.
func NewHTML(r io.Reader, m *origin.Matcher, src io.Closer, opt Options) *HTML {
	st := opt.Stats
	if st == nil {
		st = NewStats(false)
	}
	return &HTML{
		z:      html.NewTokenizer(r),
		m:      m,
		stats:  st,
		dryRun: opt.DryRun,
		src:    src,
	}
}

// rawTextElement reports elements whose content the tokenizer returns as a
// single text token rather than parsing.
func rawTextElement(n string) bool {
	switch n {
	case "script", "style", "textarea", "title", "iframe",
		"noembed", "noframes", "noscript", "plaintext", "xmp":
		return true
	}
	return false
}

// structuredAttr names the values that need parsing rather than plain origin
// substitution (PLAN §5.2). M1 substitutes plainly and counts them; M3 splits on
// the separators.
func structuredAttr(name string) bool {
	switch name {
	case "srcset", "imagesrcset", "ping", "srcdoc", "content":
		return true
	}
	return false
}

// rewriteValue is the single seam every value passes through.
func (w *HTML) rewriteValue(surface, name string, base int, v []byte) []byte {
	if name != "" && structuredAttr(name) {
		w.stats.Structured(name)
	}
	out, events := w.m.Rewrite(v, surface, w.stats.Explain())
	w.stats.Record(surface, base, events)
	if w.dryRun {
		return v
	}
	return out
}

// rewriteTag splices new attribute values into a start tag, copying everything
// between them verbatim. Attribute order, quoting, whitespace and case all
// survive, because nothing is rebuilt — only value byte ranges are replaced.
func (w *HTML) rewriteTag(raw []byte, tagOff int) []byte {
	attrs := scanAttrs(raw)
	var out bytes.Buffer
	prev := 0
	for _, a := range attrs {
		if a.ValueStart < 0 {
			continue
		}
		name := strings.ToLower(string(raw[a.NameStart:a.NameEnd]))
		val := raw[a.ValueStart:a.ValueEnd]
		nv := w.rewriteValue(SurfaceHTMLAttr, name, tagOff+a.ValueStart, val)
		if bytes.Equal(nv, val) {
			continue
		}
		out.Write(raw[prev:a.ValueStart]) // everything before this value, verbatim
		out.Write(nv)
		prev = a.ValueEnd
	}
	if prev == 0 {
		return raw // untouched: the same bytes, not a copy
	}
	out.Write(raw[prev:])
	return out.Bytes()
}

func (w *HTML) Read(p []byte) (int, error) {
	for w.pend.Len() == 0 && !w.done {
		tt := w.z.Next()
		if tt == html.ErrorToken {
			// Both halves, and in this order. When the tokenizer hits EOF part
			// way through a tag, the partial tag is in Raw() and Buffered() is
			// empty — so emitting only Buffered() silently drops those bytes.
			// Measured before this was fixed: 129 of the 244 prefixes of an
			// ordinary document lost bytes, with exit status 0 and no
			// diagnostic, which also breaks test 24 for any truncated input.
			w.pend.Write(w.z.Raw())
			w.pend.Write(w.z.Buffered())
			w.done = true

			// io.EOF is the ordinary end of a body. Anything else is a real
			// failure — an upstream that closed early, a read error — and
			// converting it to io.EOF turns a *detectable* truncation into an
			// undetectable one, because the rewritten response is chunked and
			// has no Content-Length for the client to check against.
			if err := w.z.Err(); err != nil && err != io.EOF {
				w.err = err
			}
			break
		}

		// Copy Raw() before touching TagName()/TagAttr().
		//
		// spike/go/full/main.go:100-105 aliased it across a TagName() call. The
		// docs make no lifetime promise about Raw() — the partition guarantee is
		// all they state — and safety today rests on TagAttr happening to
		// allocate before its in-place unescape, which is an implementation
		// detail. The slices from Text()/TagName()/TagAttr() *are* documented to
		// change on the next Next(), so none of them are retained either.
		raw := append([]byte(nil), w.z.Raw()...)
		off := w.inOff
		w.inOff += len(raw)

		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := w.z.TagName()
			// Self-closing too: x/net/html sets its own raw-text state for
			// `<script/>` and returns SelfClosingTagToken, so gating on
			// StartTagToken alone left that spelling's contents unscanned —
			// and inline script is where the JS URLs actually are (§5.2).
			if rawTextElement(string(name)) {
				w.rawText = string(name)
			}
			w.pend.Write(w.rewriteTag(raw, off))
		case html.EndTagToken:
			w.rawText = ""
			w.pend.Write(raw)
		case html.TextToken:
			// A raw-text element's content arrives as a single token — a 700 KB
			// inline script is one token — so it is scanned directly, with no
			// accumulation. This is where the CSS and JS URLs actually are.
			switch w.rawText {
			case "script":
				w.pend.Write(w.rewriteValue(SurfaceInlineScript, "", off, raw))
			case "style":
				w.pend.Write(w.rewriteValue(SurfaceInlineStyle, "", off, raw))
			default:
				w.pend.Write(raw)
			}
		default:
			w.pend.Write(raw)
		}
	}
	if w.pend.Len() == 0 && w.done {
		if w.err != nil {
			return 0, w.err
		}
		return 0, io.EOF
	}
	return w.pend.Read(p)
}

// Close closes the underlying source, if there is one.
func (w *HTML) Close() error {
	if w.src != nil {
		return w.src.Close()
	}
	return nil
}
