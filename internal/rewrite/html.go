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
	err     error // a real read failure, surfaced once pending bytes are out
	inOff   int   // cumulative input-stream offset, for --explain
	outOff  int   // cumulative output-stream offset, to map the sweep's finds back
	marks   []mark
	markCur int
	maxPend int // high-water mark of the token buffer, for test 13
	tail    io.Reader
	attrs   []Attr // scratch for scanAttrsInto, reused across tags
	hosts   *hostReplacer
	xmlEnt  bool
	foreign int // depth inside <svg>/<math>, where references are decoded
}

// mark records that output offset out corresponds to input offset in, from
// there until the next mark. One is appended per length-changing token — not
// per token — so the list is as long as the number of rewrites.
type mark struct{ out, in int }

// InputOffset maps an offset in this stage's output back to the offset in its
// input, so §4.4's sweep can report a straggler where it sits in the *source*
// document rather than in the rewritten stream it actually scans.
//
// Without it the two halves of one --explain event list use different
// coordinate systems: the structured pass reports input offsets, and the
// straggler's drifts by the total length change so far — on a page with 1000
// rewrites of "https://www.acmecorp.fi" to a nine-byte-longer variant, by 9000
// bytes. Queries arrive in increasing order from the sweep's own goroutine, so
// a cursor walks the list and consumed marks are compacted away.
func (w *HTML) InputOffset(out int) int {
	for w.markCur < len(w.marks) && w.marks[w.markCur].out <= out {
		w.markCur++
	}
	if w.markCur > 512 {
		w.marks = append(w.marks[:0], w.marks[w.markCur:]...)
		w.markCur = 0
		for w.markCur < len(w.marks) && w.marks[w.markCur].out <= out {
			w.markCur++
		}
	}
	if w.markCur == 0 {
		return out // before the first rewrite the streams are aligned
	}
	m := w.marks[w.markCur-1]
	return m.in + (out - m.out)
}

// write emits a token's output and records the correspondence when the token
// changed length.
func (w *HTML) write(inStart, inLen int, out []byte) {
	w.outOff += len(out)
	if len(out) != inLen {
		w.marks = append(w.marks, mark{w.outOff, inStart + inLen})
	}
	w.pend.Write(out)
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
	// XMLEntities says the document is parsed by an XML parser, which decodes
	// character references inside <script> and <style> where an HTML parser does
	// not — the reason XHTML scripts need CDATA. Set for
	// application/xhtml+xml; without it a reference-encoded origin in an XHTML
	// script was byte-identical out and fetched by the browser.
	XMLEntities bool
}

// NewResponseBody is the full response-side pipeline: the tokenizer-based
// rewriter, then §4.4's straggler sweep as a backstop.
//
// Both the proxy and `hostshift rewrite` compose it through this one function,
// which is what keeps test 27 — the filter and the proxy produce the same bytes
// — an assertion about a shared code path rather than a coincidence.
func NewResponseBody(r io.Reader, m *origin.Matcher, src io.Closer, opt Options) io.ReadCloser {
	h := NewHTML(r, m, src, opt)
	// Dry run does not sweep, and cannot.
	//
	// The sweep is a re-scan of *rewritten* output: every canonical origin it
	// finds is one the structured pass missed. Under --dry-run the structured
	// pass deliberately emits the input unchanged, so the sweep re-scans the
	// original document and reports every origin on the page as a straggler —
	// roughly a thousand WARNs on a corpus page, each claiming a bug that does
	// not exist, and every counter doubled. §5.8 calls --dry-run the mode you
	// point at a live canonical checkout to assess a new site, which is exactly
	// when a straggler report has to mean something.
	//
	// Making it mean something would take feeding the sweep the rewritten bytes
	// while emitting the original ones, i.e. buffering the whole body. The
	// census is dropped instead, and WriteReport says so rather than printing a
	// zero that reads like proof of coverage.
	if opt.NoSweep || opt.DryRun {
		h.stats.SweepSkipped()
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
		hosts:  newHostReplacer(m),
		xmlEnt: opt.XMLEntities,
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

// rawTextNames are the elements whose content the tokenizer returns as a single
// text token rather than parsing. The string form is what w.rawText holds; the
// byte form is what a tag name is compared against, without allocating.
var rawTextNames = []string{
	"script", "style", "textarea", "title", "iframe",
	"noembed", "noframes", "noscript", "plaintext", "xmp",
}

var rawTextNameBytes = func() [][]byte {
	out := make([][]byte, len(rawTextNames))
	for i, s := range rawTextNames {
		out[i] = []byte(s)
	}
	return out
}()

// rawTextElement returns the canonical name when name is a raw-text element,
// and "" otherwise. Case-folding here rather than lowercasing into a string is
// what keeps a start tag from allocating at all.
func rawTextElement(name []byte) string {
	for i, s := range rawTextNameBytes {
		if len(name) == len(s) && bytes.EqualFold(name, s) {
			return rawTextNames[i]
		}
	}
	return ""
}

// structuredAttrNames are the values §5.2 listed as needing their grammar
// parsed. M3 established that none of them does — anchoring finds origins
// wherever they sit — so these are counted for visibility, not parsed.
var structuredAttrNames = [][]byte{
	[]byte("srcset"), []byte("imagesrcset"), []byte("ping"), []byte("srcdoc"), []byte("content"),
}

// Every attribute gets the locator, with no allow-list.
//
// The list existed because the pass used to normalise whole values, deleting
// whitespace — which is content in a title and a separator in a srcset. It
// stopped doing that: it replaces the matched host's byte range and copies
// everything else. Keeping the list only produced a second class of missed
// attribute, on exactly the surface §5.2 already names —
// `data-large_image="https:\\www.example.fi/a.jpg"` came out byte-identical
// while `foldedHostLeak` and `cssEscapeLeak`, which never had a list, rewrote
// the same attribute. The WooCommerce gallery assigns that value to an img.src.

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
	// An attribute value is a value; a text node, a comment and the contents of
	// a raw-text element are prose. The distinction is one byte wide — whether
	// a trailing root dot at the end of the buffer is the host's root label or
	// a full stop — and only the caller can make it. See Matcher.RewriteText.
	rw := w.m.RewriteText
	if surface == SurfaceHTMLAttr {
		rw = w.m.Rewrite
	}
	out, events := rw(v, surface, w.stats.Explain())
	w.stats.Record(surface, base, events)
	// `value` is the same distinction RewriteText draws above: in an attribute a
	// trailing dot is the host's root label, in prose it is a full stop.
	value := surface == SurfaceHTMLAttr
	if surface == SurfaceHTMLAttr {
		out = w.urlLeaks(base, out)
	} else {
		// Every other surface gets the locator too. It ran on attributes alone,
		// so every *ASCII* URL-parser shape — `https:\h`, `https:///h`,
		// `http:h`, a tab in the host, `u@h`, `%2e` for a dot — went out
		// untouched in an inline script, an inline stylesheet, a text node and a
		// comment, with the census reporting a clean page. §5.2 calls inline
		// script and style Tier 1 and "where the CSS and JS URLs actually are",
		// and `fetch("https://www%2eexample%2efi/a")` is a production request
		// carrying the developer's session.
		out = w.normaliseURLLeak(surface, base, out, false)
	}
	// And a host that only folds onto a canonical one — a soft hyphen, fullwidth
	// letters, U+3002 for the dots, NFD — shares no bytes with its pattern on any
	// surface either.
	out = w.foldedHostLeak(surface, base, out, value)
	// An XML parser decodes references inside script and style; an HTML parser
	// does not. Attribute values are already handled by decodeEntityLeak on both.
	if (w.xmlEnt || w.foreign > 0) &&
		(surface == SurfaceInlineScript || surface == SurfaceInlineStyle ||
			surface == SurfaceRawText || surface == SurfaceText) {
		out = w.refsLeak(surface, base, out, false)
	}
	// Percent-decoding, on every surface: an encoding composed with another one
	// hides from all three of the engine's models at once. A JSON-escaped URL
	// that is then percent-encoded — WooCommerce's inline
	// `JSON.parse(decodeURIComponent("…"))` blobs — was invisible to the byte
	// matcher, to the locator and to the census alike.
	//
	// `value` is threaded here like everywhere else. It was hardcoded true, so
	// the percent spelling took the full stop at the end of a sentence into the
	// host — `See https%3A%2F%2Fwww.example.fi. Thanks` lost the dot — while the
	// plain and JSON spellings of the same sentence correctly kept it.
	out = w.percentLeak(surface, base, out, value)
	// CSS unescapes before the URL parser runs, so a style surface needs that
	// view too — see stripForCSS.
	if surface == SurfaceInlineStyle || (surface == SurfaceHTMLAttr && len(name) == 5 && bytes.EqualFold(name, []byte("style"))) {
		out = w.cssEscapeLeak(surface, base, out)
		// And the two decodes composed, where the parser performs both: an
		// attribute value always has its references decoded, and a `<style>`
		// element does inside `<svg>`/`<math>` or in XHTML — the same gate the
		// reference pass above uses.
		if surface == SurfaceHTMLAttr || w.xmlEnt || w.foreign > 0 {
			out = w.refsCSSLeak(surface, base, out)
		}
	}
	// Every surface, because a host that only folds onto a canonical one — a
	// soft hyphen, fullwidth letters, U+3002 for the dots, NFD — shares no bytes
	// with its pattern anywhere, not just in a URL attribute.

	if w.dryRun {
		return v
	}
	return out
}

// decodeEntityLeak closes the one gap between the matcher's encodings and the
// browser's: character references inside an attribute value.
//
// §5.3 models three encodings — raw "//", JSON "\/\/" and percent "%2F%2F" —
// and none of them covers href="https:&#47;&#47;www.example.fi/x", which a
// browser decodes before it resolves the URL and then dereferences straight to
// production. That is test 28, which is safety-critical: an agent following
// that link issues writes against the live site. Pattern variants cannot close
// it, because "&#47;", "&#047;", "&#x2f;" and "&sol;" are one family of
// unbounded size — leading zeros alone see to that.
//
// So the value is decoded and re-matched. If the decoded form carries an origin
// the raw form did not, the decoded-and-rewritten text replaces the value. That
// re-serialises the value, which §5.2 otherwise forbids; it is confined to
// values that would *otherwise leak*, so it never runs on a page that is
// already correct, and byte-identity is untouched because the identity map
// rewrites nothing to begin with.
//
// Attribute values only: inside <script> and <style> the browser does not
// decode references, so there is nothing there to decode.
// urlLeaks runs both catchers, over one decode.
//
// They have to see the same bytes. decodeEntityLeak returned *v* — the value as
// written — whenever the decoded form did not itself rewrite, so the URL pass
// only ever saw the undecoded text, and every shape needing decode-then-parse
// went out untouched: `https:&#47;&#47;www.example&#9;.fi/x` resolves to
// production and was the exact case the comment here claimed was covered.
func (w *HTML) urlLeaks(base int, v []byte) []byte {
	dec, decoded := decodeURLRefs(v)
	// cur is what the locator sees: the decoded form, or the decoded *and
	// entity-rewritten* form when that pass fired. Returning early on the
	// entity pass's success skipped the locator for the whole value, so a second
	// origin in the same value went out untouched —
	// `srcset="https:&#47;&#47;a/1 1x, https:\a/2 2x"` rewrote the first entry
	// and left the second dereferencing production, while --json reported a
	// successful rewrite on a value that still leaked.
	cur := dec
	if decoded {
		if out := w.decodeEntityLeak(base, v, dec); out != nil {
			cur = out
		}
	}
	if out := w.normaliseURLLeak(SurfaceHTMLObfuscated, base, cur, true); !bytes.Equal(out, cur) {
		return out
	}
	// Three paths cover a reference-encoded origin in an attribute, and that is
	// deliberate rather than accidental: decodeEntityLeak above, this view, and
	// normaliseURLLeak — which runs on `dec`, the decoded form, so it locates one
	// too. Disabling any one of the three still rewrites; disabling all three
	// leaks. The view is the one that survives a value decodeURLRefs declines.
	//
	// The reference *view*, which needs no fusing guard because it emits nothing.
	//
	// decodeURLRefs declines an entire value whenever any fragment in it would
	// fuse into a new reference — and that fragment can be anywhere, so a
	// `&#6`+`&#48;`+`;` sequence in a query string disabled decoding for an
	// ordinary `https:&#47;&#47;canonical/` in the same attribute, which then went
	// out live. The guard is right about *splicing* a decoded value back; it has
	// nothing to say about locating a host and replacing its byte range, where
	// the decoded bytes never leave the view.
	if w.hosts != nil {
		if out := w.hosts.refsOnly(cur, true); !bytes.Equal(out, cur) {
			w.stats.Record(SurfaceHTMLEntity, base, []origin.Event{{
				Offset: base, Surface: SurfaceHTMLEntity, Action: origin.ActionRewrote,
			}})
			return out
		}
	}
	if !bytes.Equal(cur, dec) {
		return cur
	}
	return v
}

func (w *HTML) decodeEntityLeak(base int, v, dec []byte) []byte {
	out, events := w.m.Rewrite(dec, SurfaceHTMLEntity, w.stats.Explain())
	// Recorded before the equality check, not after. Returning early skipped the
	// *skips*, so a decoded value carrying an unanchored near-miss —
	// `https:&#47;&#47;www.example.fi.evil/x` — counted zero candidates and zero
	// skips, where the raw spelling of the same thing counts both. That is the
	// state matcher.go's emit comment says was fixed: candidates == rewrites
	// reads as "nothing was skipped", on the one surface where a non-zero count
	// is documented as the signal that content is storing origins in a form
	// §5.3 does not model.
	w.stats.Record(SurfaceHTMLEntity, base, events)
	if bytes.Equal(out, dec) {
		return nil // nothing here; the caller tries the URL pass on dec
	}
	return out
}

// rewriteTag splices new attribute values into a start tag, copying everything
// between them verbatim. Attribute order, quoting, whitespace and case all
// survive, because nothing is rebuilt — only value byte ranges are replaced.
func (w *HTML) rewriteTag(raw []byte, tagOff int) []byte {
	w.attrs = scanAttrsInto(w.attrs[:0], raw)
	attrs := w.attrs
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
			err := w.z.Err()

			if err == html.ErrBufferExceeded {
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
				// Raw() and Buffered(), for the same reason the EOF path below
				// gives — and here it is not an edge case but the main one.
				//
				// x/net/html's readByte advances raw.end *before* it tests
				// maxBuf, so at the error the oversized token sits in Raw() and
				// Buffered() holds only read-ahead. A text token is returned as
				// a partial TextToken first, so text, <script> and comments
				// survived; a *tag* token errors from inside readStartTag with
				// the bytes still in Raw(), so emitting Buffered() alone deleted
				// exactly MaxToken bytes. At the shipped 4 MiB cap that is a
				// 5 MB page arriving 4 MiB short, status 200, no Content-Length
				// to check it against, the opening <img src="data:image/png;
				// base64, gone and the rest of its value rendered as visible
				// text. An inlined LCP image or a multi-MB Elementor
				// data-settings attribute is all it takes, and it broke test 24.
				head := append([]byte(nil), w.z.Raw()...)
				head = append(head, w.z.Buffered()...)
				w.tail = io.MultiReader(bytes.NewReader(head), w.raw)

				// The tail bypasses write(), so one last mark pins the mapping
				// for everything after it: from here the two streams run 1:1
				// again, and without this the sweep reports stragglers in the
				// passthrough at output offsets — 4 MiB adrift in the case
				// above.
				w.marks = append(w.marks, mark{w.outOff, w.inOff})
				w.done = true
				return w.tail.Read(p)
			}

			// Both halves, and in this order. When the tokenizer hits EOF part
			// way through a tag, the partial tag is in Raw() and Buffered() is
			// empty — so emitting only Buffered() silently drops those bytes.
			// Measured before this was fixed: 129 of the 244 prefixes of an
			// ordinary document lost bytes, with exit status 0 and no
			// diagnostic, which also breaks test 24 for any truncated input.
			tail := w.pend.Len()
			w.pend.Write(w.z.Raw())
			w.pend.Write(w.z.Buffered())
			// Pass-through, so the two streams advance together and no mark is
			// needed; the counters still have to move or InputOffset would map
			// the tail against the last rewrite before it.
			n := w.pend.Len() - tail
			w.inOff += n
			w.outOff += n

			// io.EOF is the ordinary end of a body. Anything else is a real
			// failure — an upstream that closed early, a read error — and
			// converting it to io.EOF turns a *detectable* truncation into an
			// undetectable one, because the rewritten response is chunked and
			// has no Content-Length for the client to check against. The token
			// cap is the one such error with a better answer than failing, and
			// it is handled above.
			if err != nil && err != io.EOF {
				w.err = err
			}
			w.done = true
			break
		}

		raw := w.z.Raw()
		off := w.inOff
		w.inOff += len(raw)

		// Raw() is used directly, and no copy is made.
		//
		// The copy existed because TagName()/TagAttr() may invalidate Raw() —
		// the docs promise only the partition guarantee, and safety rested on
		// TagAttr happening to allocate before its in-place unescape, which is
		// an implementation detail. spike/go/full/main.go:100-105 aliased across
		// exactly that and was the reason for the hedge.
		//
		// Neither is called any more. The tag name comes from the raw bytes
		// (tagNameOf), which is where readTagName would have found it, and the
		// attribute spans always came from scanAttrs. So the hazard is gone
		// rather than guarded against — and with it two allocations per start
		// tag, since TagName() is bytes.ReplaceAll and Go's bytes.Replace copies
		// even when it replaces nothing. Text()/Raw() are still never retained
		// past the next Next().

		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			// Self-closing too: x/net/html sets its own raw-text state for
			// `<script/>` and returns SelfClosingTagToken, so gating on
			// StartTagToken alone left that spelling's contents unscanned —
			// and inline script is where the JS URLs actually are (§5.2).
			if name := rawTextElement(tagNameOf(raw)); name != "" {
				w.rawText = name
			}
			// Foreign content: inside <svg> and <math> the HTML tokenizer never
			// enters the raw-text states, so a browser decodes character
			// references in <style>, <script> and <title> there — verified in
			// Chrome, where an inline `<svg><script>` with a reference-encoded
			// origin *ran*, and an `<svg><style>` fetched one. x/net/html's
			// tokenizer is context-free and hands those back as raw text either
			// way, so nothing downstream could tell the difference. The scoping
			// was by content type where it belongs on whether the element is in
			// foreign content: the same SVG served standalone was rewritten and
			// inlined in a page was not.
			if tt == html.StartTagToken {
				if n := string(bytes.ToLower(tagNameOf(raw))); n == "svg" || n == "math" {
					w.foreign++
				}
			}
			w.write(off, len(raw), w.rewriteTag(raw, off))
		case html.EndTagToken:
			w.rawText = ""
			if n := string(bytes.ToLower(endTagNameOf(raw))); n == "svg" || n == "math" {
				if w.foreign > 0 {
					w.foreign--
				}
			}
			w.write(off, len(raw), raw)
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
				w.write(off, len(raw), w.rewriteValue(SurfaceText, nil, off, raw))
			case "script":
				w.write(off, len(raw), w.rewriteValue(SurfaceInlineScript, nil, off, raw))
			case "style":
				w.write(off, len(raw), w.rewriteValue(SurfaceInlineStyle, nil, off, raw))
			default:
				w.write(off, len(raw), w.rewriteValue(SurfaceRawText, nil, off, raw))
			}
		case html.CommentToken:
			// Not dereferenceable by the browser, but the fleet puts real URLs
			// here: sage-cachetags emits "<!-- sage-cachetags Url: https://… -->"
			// on every cached page, and the M6 pilot found 20-odd per crawl
			// going to the sweep. §4.4 wants every straggler to be a bug in the
			// structured pass, so this belongs here rather than in the backstop.
			w.write(off, len(raw), w.rewriteValue(SurfaceComment, nil, off, raw))

		default:
			w.write(off, len(raw), raw)
		}
	}
	if n := w.pend.Len(); n > w.maxPend {
		w.maxPend = n
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
