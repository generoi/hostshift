package rewrite

import (
	"bytes"
	"io"
	"log/slog"

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
	log     *slog.Logger
	dryRun  bool
	src     io.Closer
	raw     io.Reader // the original source, for the passthrough fallback
	rawText string
	pend    bytes.Buffer
	done    bool
	inOff   int // cumulative input-stream offset, for --explain
	maxPend int // high-water mark of the token buffer, for test 13
	tail    io.Reader
}

// DefaultMaxToken caps the tokenizer's buffer, which bounds memory by the size
// of the largest single token rather than by the response size (test 13).
//
// It has to be generous: a raw-text element arrives as one token, and a 700 KB
// inline script is normal. Exceeding it is not fatal — see Read.
const DefaultMaxToken = 4 << 20

// Options configures a rewriter.
type Options struct {
	// DryRun computes and counts every rewrite but emits the input unchanged
	// (PLAN §5.8) — safe to point at a live canonical checkout.
	DryRun bool
	// NoSweep disables PLAN §4.4's straggler backstop. It exists to *measure*
	// the structured pass, not to run without a net.
	NoSweep bool
	Stats   *Stats
	Log     *slog.Logger
	// MaxToken caps the tokenizer's buffer. Zero means DefaultMaxToken.
	MaxToken int
}

// NewResponseBody is the full response-side pipeline: the tokenizer-based
// rewriter, then §4.4's straggler sweep as a backstop.
//
// Both the proxy and `hostshift rewrite` compose it through this one function,
// which is what keeps test 27 — the filter and the proxy produce the same bytes
// — an assertion about a shared code path rather than a coincidence.
func NewResponseBody(r io.Reader, m *origin.Matcher, src io.Closer, opt Options) io.ReadCloser {
	h := NewHTML(r, m, src, opt)
	if opt.NoSweep {
		return h
	}
	return NewSweep(h, m, h, opt)
}

// NewHTML wraps r. If src is non-nil, Close closes it — the proxy passes the
// upstream body so the rewriter can stand in for it as a ReadCloser.
func NewHTML(r io.Reader, m *origin.Matcher, src io.Closer, opt Options) *HTML {
	st := opt.Stats
	if st == nil {
		st = NewStats(false)
	}
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	maxTok := opt.MaxToken
	if maxTok == 0 {
		maxTok = DefaultMaxToken
	}
	z := html.NewTokenizer(r)
	z.SetMaxBuf(maxTok)
	return &HTML{
		z:      z,
		m:      m,
		stats:  st,
		log:    log,
		dryRun: opt.DryRun,
		src:    src,
		raw:    r,
	}
}

// MaxBuffered is the high-water mark of the token buffer, in bytes. Test 13
// asserts it is bounded by the largest token rather than by the response size.
func (w *HTML) MaxBuffered() int { return w.maxPend }

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

// structuredAttrNames are the values §5.2 listed as needing their grammar
// parsed. M3 established that none of them does — anchoring finds origins
// wherever they sit — so these are counted for visibility, not parsed.
var structuredAttrNames = [][]byte{
	[]byte("srcset"), []byte("imagesrcset"), []byte("ping"), []byte("srcdoc"), []byte("content"),
}

// structuredAttr matches on the raw name bytes, case-insensitively. Lowercasing
// every attribute name to a string first cost one allocation per attribute —
// 37,280 of them across the corpus — for a check that five byte comparisons
// answer.
func structuredAttr(name []byte) []byte {
	for _, s := range structuredAttrNames {
		if len(name) == len(s) && bytes.EqualFold(name, s) {
			return s
		}
	}
	return nil
}

// rewriteValue is the single seam every value passes through.
func (w *HTML) rewriteValue(surface string, name []byte, base int, v []byte) []byte {
	if s := structuredAttr(name); s != nil {
		w.stats.Structured(string(s))
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
		name := raw[a.NameStart:a.NameEnd]
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
	if w.tail != nil {
		return w.tail.Read(p)
	}
	for w.pend.Len() == 0 && !w.done {
		tt := w.z.Next()
		if tt == html.ErrorToken {
			// Buffered() is whatever the tokenizer had read but not tokenised.
			// Emitting it is what makes truncated input round-trip byte-exactly.
			buffered := w.z.Buffered()

			if err := w.z.Err(); err != nil && err != io.EOF {
				// A single token larger than the cap. Headers are already sent
				// by now, so this cannot become an error response (PLAN §5.7) —
				// and aborting the connection would be a worse answer than a
				// page with one unrewritten region.
				//
				// Instead the remainder streams through untouched. §4.4's
				// straggler sweep sits downstream of this reader, so origins in
				// the passthrough tail are still caught and reported rather than
				// leaking.
				w.log.Warn("token exceeds the buffer cap; the remainder of this response is passed through unparsed",
					"err", err, "offset", w.inOff)
				w.stats.Record(SurfaceHTMLAttr, w.inOff, []origin.Event{{
					Surface: SurfaceHTMLAttr, Action: origin.ActionSkipped,
					Reason: origin.ReasonSizeCap,
				}})
				w.tail = io.MultiReader(bytes.NewReader(append([]byte(nil), buffered...)), w.raw)
				w.done = true
				if w.pend.Len() > 0 {
					break
				}
				return w.tail.Read(p)
			}

			w.pend.Write(buffered)
			w.done = true
			break
		}

		raw := w.z.Raw()
		off := w.inOff
		w.inOff += len(raw)

		// Copy Raw() before touching TagName()/TagAttr(), and *only* then.
		//
		// spike/go/full/main.go:100-105 aliased it across a TagName() call. The
		// docs make no lifetime promise about Raw() — the partition guarantee is
		// all they state — and safety today rests on TagAttr happening to
		// allocate before its in-place unescape, which is an implementation
		// detail. The slices from Text()/TagName()/TagAttr() *are* documented to
		// change on the next Next(), so none of them are retained either.
		//
		// Every other token type is written straight to the pending buffer,
		// which copies, and TagName/TagAttr are never called for them — so the
		// defensive copy is pure garbage there. Restricting it to start tags
		// removes roughly a third of the allocations on a page with no rewrites.
		if tt == html.StartTagToken || tt == html.SelfClosingTagToken {
			raw = append([]byte(nil), raw...)
		}

		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := w.z.TagName()
			if tt == html.StartTagToken && rawTextElement(string(name)) {
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
			//
			// Every raw-text element is scanned, not just script and style. The
			// tokenizer hands back the *markup* inside <noscript>, <textarea>,
			// <iframe> and <svg><title> as opaque text, so a URL in an <a href>
			// there is invisible to the attribute scan. Scanning only script and
			// style left those to §4.4's sweep, which is meant to be a backstop
			// rather than a load-bearing part; the corpus turned up a real
			// <noscript> case.
			//
			// Anchored matching is what makes this safe on prose-bearing
			// elements like <title>: it can only match a real origin, never a
			// bare hostname (test 28).
			switch w.rawText {
			case "":
				// Ordinary body text. The M6 pilot found real pages carrying
				// "https://host/path" in visible prose — a privacy-policy
				// paragraph quoting its own URL — which the sweep was then
				// catching. Under production-canonical that is precisely the
				// hazard §4.4 opens with: a developer copy-pastes it and lands
				// on live production.
				//
				// §4.4 already accepts the consequence: "a page that
				// intentionally links to production, as a URL, is rewritten too.
				// On a development clone that is almost always what you want."
				// Anchoring is what keeps test 28's exclusion intact — a bare
				// hostname in prose has no scheme and cannot match.
				w.pend.Write(w.rewriteValue(SurfaceText, nil, off, raw))
			case "script":
				w.pend.Write(w.rewriteValue(SurfaceInlineScript, nil, off, raw))
			case "style":
				w.pend.Write(w.rewriteValue(SurfaceInlineStyle, nil, off, raw))
			default:
				w.pend.Write(w.rewriteValue(SurfaceRawText, nil, off, raw))
			}
		case html.CommentToken:
			// Not dereferenceable by the browser, but the fleet puts real URLs
			// here: sage-cachetags emits "<!-- sage-cachetags Url: https://… -->"
			// on every cached page, and the M6 pilot found 20-odd per crawl
			// going to the sweep. §4.4 wants every straggler to be a bug in the
			// structured pass, so this belongs here rather than in the backstop.
			w.pend.Write(w.rewriteValue(SurfaceComment, nil, off, raw))

		default:
			w.pend.Write(raw)
		}
	}
	if n := w.pend.Len(); n > w.maxPend {
		w.maxPend = n
	}
	if w.pend.Len() == 0 && w.done {
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
