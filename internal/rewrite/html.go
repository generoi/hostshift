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
	// foreignNS is the open <svg>/<math> elements, innermost last. Its length is
	// the old `foreign` depth; its last entry is the vocabulary an element name
	// has to be read in.
	//
	// Each entry carries both, because the two differ: inside foreign content a
	// start tag is inserted *in the namespace of the adjusted current node*
	// (13.2.6.5), so the `<math>` in `<svg><math>` is an SVG element and its
	// `<mi>` is not a MathML text integration point. Round 62 pushed the tag
	// name as the vocabulary and read `<svg><math><mi><script>` as HTML rules,
	// withholding a decode the browser performs. The name is kept because end
	// tags match on it.
	foreignNS []foreignEl
	// foreignObjectAt is the <svg>/<math> depth at which an HTML integration
	// point resumed HTML rules, or 0 when none has.
	//
	// A depth and not a count, because the parser has a stack and this file has
	// a streaming tokenizer. Round 60 counted `<foreignObject>` up and down and
	// got both halves wrong. An integration point is *re-entrant* — an <svg>
	// inside it puts the parser back in foreign content — so a nested
	// `<svg><script>` had its references withheld and a canonical origin went to
	// the browser inside a script the page runs. And the counter only came down
	// on an explicit end tag, while `</svg>` and end-of-document close a
	// foreignObject implicitly, so one unbalanced tag anywhere disarmed
	// reference decoding for the whole rest of the response.
	//
	// `w.foreign` has always had the unbalanced shape too and it is harmless
	// there, because an unbalanced `<svg>` leaves the model *over*-decoding —
	// the direction §4.4 picks on purpose. Round 60 added a counter with the
	// same shape and the opposite sign, which inverted the failure into a leak.
	// Comparing depths restores the sign: anything that loses track resolves to
	// "still foreign", which over-decodes.
	foreignObjectAt int
	// mathTextPoint records whether the integration point that resumed HTML
	// rules was a MathML text one, the only kind with a carve-out.
	mathTextPoint bool
	// rawTextForeign records whether the open raw-text element is itself a
	// foreign one, decided at its start tag.
	rawTextForeign bool
	// foreignSpan marks a byte range inside one raw-text token as foreign when
	// the element stack cannot say so.
	//
	// `<svg><title><svg><style>` re-enters foreign content *inside* an
	// integration point, and the whole title arrives as a single opaque token —
	// x/net/html's tokenizer switches to RCDATA on `<title>` whatever the
	// namespace, while its parser suppresses that switch in foreign content. So
	// the nesting is invisible to the element stack by construction, and the
	// `<style>` in there is an SVG one whose character references the browser
	// decodes and whose `@import` it fetches.
	foreignSpan bool
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

// integrationPointIn reports whether an element name is an integration point *in
// the vocabulary it appears in* — a place where a foreign-content parser goes
// back to HTML rules.
//
// The namespace is half the rule and round 61 modelled only the names, so
// `<svg><mtext>` — an ordinary SVG element the parser stays foreign inside —
// was read as HTML rules resumed and a reference decode the browser performs was
// withheld. Chrome builds a live stylesheet from an `@import` there.
//
// `annotation-xml` is deliberately absent: it is one only when its encoding says
// so, which the caller that cares checks with htmlEncoding.
func integrationPointIn(name, ns string) bool {
	switch ns {
	case "svg":
		switch name {
		case "foreignobject", "desc", "title":
			return true
		}
	case "math":
		switch name {
		case "mi", "mo", "mn", "ms", "mtext":
			return true
		}
	}
	return false
}

// foreignEl is one open <svg>/<math>: the name its end tag has to match, and
// the vocabulary its children are read in. They are not the same thing.
type foreignEl struct {
	name string
	ns   string
}

// currentNS is the vocabulary of the innermost open <svg>/<math>, or "".
func (w *HTML) currentNS() string {
	if len(w.foreignNS) == 0 {
		return ""
	}
	return w.foreignNS[len(w.foreignNS)-1].ns
}

// inForeignContent reports whether the parser is in foreign content here: an
// <svg>/<math> is open and no integration point below this depth has resumed
// HTML rules. An <svg> *inside* a foreignObject is foreign again, which a count
// could not express.
func (w *HTML) inForeignContent() bool {
	return len(w.foreignNS) > 0 &&
		(w.foreignObjectAt == 0 || len(w.foreignNS) > w.foreignObjectAt)
}

// popForeign applies 13.2.6.5's end-tag walk to the open vocabularies: look
// from the innermost outward for one whose name matches, and pop through it.
//
// A mismatch pops *nothing*. Round 62 popped the innermost unconditionally, so
// a stray `</math>` inside an open `<svg>` closed the svg in the model while the
// parser — which walks down, finds no match, reaches an HTML element and
// reprocesses the token under HTML rules, where it is dropped — stayed in
// foreign content and kept decoding. One unbalanced tag was a leak.
//
// The walk still has to look past the innermost, because a match deeper down
// does pop everything above it: `<math><svg></math>` closes both, and the
// `<script>` after it is an HTML element outside the math.
func (w *HTML) popForeign(name string) {
	for i := len(w.foreignNS) - 1; i >= 0; i-- {
		if w.foreignNS[i].name != name {
			continue
		}
		w.foreignNS = w.foreignNS[:i]
		// `</svg>` closes an open foreignObject implicitly (13.2.6.5). Without
		// this the mark outlived the element it belonged to and disarmed the
		// rest of the document.
		if len(w.foreignNS) < w.foreignObjectAt {
			w.foreignObjectAt = 0
		}
		return
	}
}

// breakoutNames is 13.2.6.5's list of HTML start tags that end foreign content.
//
// Taken from the spec and checked name by name against html.Parse: every one of
// these puts a following `<script>` in the HTML namespace, and the near misses
// that are *not* on it — `section`, `article`, `aside`, and any SVG element —
// leave the parser foreign. `font` is on the list only with a color, face or
// size attribute, which is the one entry that is not a name test.
var breakoutNames = map[string]bool{
	"b": true, "big": true, "blockquote": true, "body": true, "br": true,
	"center": true, "code": true, "dd": true, "div": true, "dl": true, "dt": true,
	"em": true, "embed": true, "h1": true, "h2": true, "h3": true, "h4": true,
	"h5": true, "h6": true, "head": true, "hr": true, "i": true, "img": true,
	"li": true, "listing": true, "menu": true, "meta": true, "nobr": true,
	"ol": true, "p": true, "pre": true, "ruby": true, "s": true, "small": true,
	"span": true, "strong": true, "strike": true, "sub": true, "sup": true,
	"table": true, "tt": true, "u": true, "ul": true, "var": true,
}

// breaksOutOfForeign reports whether this start tag ends foreign content.
func breaksOutOfForeign(name string, raw []byte) bool {
	if breakoutNames[name] {
		return true
	}
	if name != "font" {
		return false
	}
	z := html.NewTokenizer(bytes.NewReader(raw))
	if z.Next() == html.ErrorToken {
		return false
	}
	for {
		k, _, more := z.TagAttr()
		switch {
		case bytes.EqualFold(k, []byte("color")),
			bytes.EqualFold(k, []byte("face")),
			bytes.EqualFold(k, []byte("size")):
			return true
		}
		if !more {
			return false
		}
	}
}

// tagStartIn is the offset of `<name` in already-lowercased b, where name ends
// at a tag-name terminator rather than anywhere.
//
// A bare prefix search calls `<style-guide>` a stylesheet, and the CSS view is
// the one view in a foreign `<title>` that does *not* decode character
// references — so an `<img src="https:&#47;&#47;canonical/x">` behind such a
// decoy went out untouched, a fetch of live production with the developer's
// session on it. The same document without the decoy is rewritten, which makes
// it a scope error and not a gap.
func tagStartIn(b []byte, name string) int {
	lit := append([]byte{'<'}, name...)
	for from := 0; ; {
		i := bytes.Index(b[from:], lit)
		if i < 0 {
			return -1
		}
		i += from
		switch e := i + len(lit); {
		case e == len(b):
			// Truncated at the token boundary. The name may continue in bytes
			// that are not here, so this is not known to be the element.
			return -1
		case b[e] == '>' || b[e] == '/' || asciiSpace(b[e]):
			return i
		}
		from = i + 1
	}
}

// openForeignBefore reports whether an <svg> or <math> is still open in this
// already-lowercased prefix of a raw-text token.
//
// Counting is enough here and a stack is not, because the only question is
// whether the parser is back in foreign content, and the two vocabularies answer
// it the same way. An unbalanced tag leaves the count high, which says "foreign"
// and over-decodes — the direction §4.4 picks when it has to pick.
func openForeignBefore(b []byte) bool {
	depth := 0
	for _, n := range [...]string{"svg", "math"} {
		for from := 0; ; {
			i := tagStartIn(b[from:], n)
			if i < 0 {
				break
			}
			depth++
			from += i + 1
		}
		for from := 0; ; {
			i := tagStartIn(b[from:], "/"+n)
			if i < 0 {
				break
			}
			depth--
			from += i + 1
		}
	}
	return depth > 0
}

// rcdataOpensBefore reports whether this already-lowercased span opens an
// element whose content is text rather than markup.
//
// Inside one of these the tokenizer would be in RCDATA and no `<style>` after it
// is an element. They are the elements that swallow markup: `title` and
// `textarea` are RCDATA, `iframe` and `noscript` are raw text.
func rcdataOpensBefore(b []byte) bool {
	for _, n := range [...]string{"title", "textarea", "iframe", "noscript"} {
		if tagStartIn(b, n) >= 0 {
			return true
		}
	}
	return false
}

// asciiSpace is the HTML tokenizer's whitespace, which is not unicode.IsSpace.
func asciiSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r'
}

// writeRawTextAroundStyles writes a raw-text token that the parser would have
// split into real elements: the content of each `<style>` gets the CSS view, and
// everything else gets the raw-text view.
//
// The tokenizer hands back `<title>` as one opaque token, so both live in the
// same bytes. Only the stylesheet is a stylesheet; treating the whole token as
// CSS unescaped `\3a ` runs in text a reader sees.
func (w *HTML) writeRawTextAroundStyles(off int, raw []byte) {
	low := bytes.ToLower(raw)
	pos := 0
	for pos < len(raw) {
		i := tagStartIn(low[pos:], "style")
		if i < 0 {
			break
		}
		i += pos
		// An RCDATA element opened before it makes this `<style>` literal text
		// rather than an element: `<svg><title><title><style>` builds an *HTML*
		// title inside the SVG one, and everything after that is its text — with
		// references decoded, which is where the origin then sits. The CSS view
		// does not decode them, so the span walk has to stop here and let the
		// raw-text view take the rest. Stopping is also the over-decode
		// direction, which is the one to fail toward.
		if rcdataOpensBefore(low[pos:i]) {
			break
		}
		gt := bytes.IndexByte(raw[i:], '>')
		if gt < 0 {
			break
		}
		// The start tag stays with the surrounding text: it is markup, not CSS.
		start := i + gt + 1
		end := len(raw)
		if j := tagStartIn(low[start:], "/style"); j >= 0 {
			end = start + j
		}
		w.write(off+pos, start-pos, w.rewriteValue(SurfaceRawText, nil, off+pos, raw[pos:start]))
		// A `<svg>` or `<math>` still open at this point puts the stylesheet back
		// in foreign content, where the browser decodes its character references
		// before the CSS tokenizer ever sees them. Unbalanced resolves to
		// "foreign", which over-decodes — §4.4's direction.
		w.foreignSpan = openForeignBefore(low[:i])
		w.write(off+start, end-start, w.rewriteValue(SurfaceInlineStyle, nil, off+start, raw[start:end]))
		w.foreignSpan = false
		pos = end
	}
	if pos < len(raw) {
		w.write(off+pos, len(raw)-pos, w.rewriteValue(SurfaceRawText, nil, off+pos, raw[pos:]))
	}
}

// htmlEncoding reports whether a MathML <annotation-xml> start tag carries an
// encoding that makes it an HTML integration point.
//
// The two spellings the parser accepts are `text/html` and
// `application/xhtml+xml`, ASCII case-insensitively (HTML 13.2.6). Anything
// else — or no encoding at all — leaves it ordinary MathML, where the parser
// stays in foreign content and decodes references as it does everywhere else
// inside <math>.
func htmlEncoding(raw []byte) bool {
	z := html.NewTokenizer(bytes.NewReader(raw))
	if z.Next() == html.ErrorToken {
		return false
	}
	for {
		k, v, more := z.TagAttr()
		if bytes.EqualFold(k, []byte("encoding")) {
			// The whole value, ASCII case-insensitively. Round 61 took any
			// attribute *containing* "encoding" and prefix-matched the value,
			// so `data-encoding="text/html"`, `encoding="text/htmlish"` and
			// `encoding="text/html; charset=utf-8"` all counted — and each of
			// them leaves the element ordinary MathML, where the parser stays
			// foreign and the browser decodes. Measured in Chrome: only the
			// exact spelling puts the child in the XHTML namespace.
			return bytes.EqualFold(v, []byte("text/html")) ||
				bytes.EqualFold(v, []byte("application/xhtml+xml"))
		}
		if !more {
			return false
		}
	}
}

// rcdataElement reports whether an element's text is RCDATA rather than RAWTEXT.
//
// The tokenizer hands both back as one text token, and rawTextNames is one list
// — but the parser has three states for them. RAWTEXT (`style`, `xmp`,
// `iframe`, `noembed`, `noframes`) decodes nothing, which is what SurfaceRawText
// is documented for. **RCDATA** (`title`, `textarea`) decodes character
// references (HTML 13.2.5.2), and `noscript` with scripting disabled is
// ordinary parsed markup, so it decodes them too. Measured: `&#47;&#47;` was
// decoded 22 times out of 22 in an `href` and 0 out of 22 in a `title`, while
// ada resolves the decoded form to the canonical either way.
//
// `noscript` is the one of the three that is dereferenced — its classic payload
// is an analytics `<img>` — so this is test 28 there and a byte-accuracy
// question in the other two. A proxy cannot know whether scripting is on, and
// decoding is the direction that rewrites rather than leaks.
func rcdataElement(name string) bool {
	switch name {
	case "title", "textarea", "noscript":
		return true
	}
	return false
}

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
// rewriteValue wraps the per-value pipeline in RepairSerialized.
//
// A serialized blob reaches the browser through an `esc_attr` hidden input, an
// `esc_textarea` or a `wp_localize_script` line, and rewriting a host inside one
// without re-emitting its `s:NN:` leaves a length PHP refuses. Doing it here
// keeps the HTML arm streamed: an attribute value and a text node are already
// handled as whole units, so nothing needs buffering that was not buffered
// before.
//
// Until this existed the HTML arm was the one direction that could not repair,
// and that asymmetry is the whole of rounds twenty-two to twenty-six: the
// browser was served a stale length, posted it back, and the request direction
// had to guess whether to believe it. With both directions repairing there is
// nothing to guess.
func (w *HTML) rewriteValue(surface string, name []byte, base int, v []byte) []byte {
	// A cheap gate first. Wrapping every attribute value and text node in the
	// walk cost 45% of the identity map's throughput — four syntaxes tried at
	// every candidate on every value — and almost no value on a page carries a
	// serialized header at all. A header is a letter, a colon and a digit; if
	// that never occurs there is nothing to repair.
	if !mayHoldSerialized(v) {
		return w.rewriteValueInner(surface, name, base, v)
	}
	return RepairSerialized(v, func(b []byte) []byte {
		return w.rewriteValueInner(surface, name, base, b)
	})
}

func (w *HTML) rewriteValueInner(surface string, name []byte, base int, v []byte) []byte {
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
	//
	// Body prose is the exception that needs no condition: an HTML parser always
	// decodes references there, so `<p>https:&#47;&#47;canonical/x` renders as a
	// URL a developer copy-pastes and lands on live production — the hazard §4.4
	// opens with, and the one the M6 pilot's privacy-policy paragraph was. The
	// gate had it firing only in foreign content, so the same bytes were
	// rewritten inside `<title>` and shipped inside `<p>`. This branch is reached
	// only from the HTML writer; a `text/plain` body never gets here, which is
	// what makes always-on right — decoding references in one really would be
	// round 60's over-rewrite.
	inForeign := w.inForeignContent() || w.foreignSpan
	if (w.xmlEnt || inForeign || surface == SurfaceText ||
		(surface == SurfaceRawText && rcdataElement(w.rawText))) &&
		(surface == SurfaceInlineScript || surface == SurfaceInlineStyle ||
			surface == SurfaceRawText || surface == SurfaceText) {
		// refsLeak alone, deliberately. The composed refs-then-CSS view would
		// also fire here, and it is the wrong tool: a `<script>` inside <svg> is
		// not CSS, so unescaping `\3a ` there would rewrite something no browser
		// resolves — the mirror of the error this whole file guards against.
		//
		// What made `<svg><script>fetch("https:&#47;&#10;&#47;canonical/x")</script>`
		// leak was not the missing composition but stripForRefs's removal pass,
		// which tested isURLStripped alone and so left a reference-spelled LF in
		// place. stripRemovals fixes it for every decoder at once, which is why
		// this surface needs nothing of its own.
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
	// And JSON's `\uXXXX`, on every surface for the same reason: a block
	// delimiter is an HTML comment, a `data-wp-*` attribute is an attribute, and
	// an inline `<script>` carries the same blob.
	out = w.jsonEscLeak(surface, base, out, value)
	// CSS unescapes before the URL parser runs, so a style surface needs that
	// view too — see stripForCSS.
	if surface == SurfaceInlineStyle || (surface == SurfaceHTMLAttr && len(name) == 5 && bytes.EqualFold(name, []byte("style"))) {
		out = w.cssEscapeLeak(surface, base, out)
		// And the two decodes composed, where the parser performs both: an
		// attribute value always has its references decoded, and a `<style>`
		// element does inside `<svg>`/`<math>` or in XHTML — the same gate the
		// reference pass above uses, which means the same `inForeign` and not a
		// second reading of the raw depth. Round 60 taught one of the two sites
		// that an integration point resumes HTML rules and left this one asking
		// the older question, so a `<style>` inside `<foreignObject>` — RAWTEXT,
		// where the parser decodes nothing — had its references decoded anyway.
		if surface == SurfaceHTMLAttr || w.xmlEnt || inForeign {
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
	// Carried forward, not returned. The comment above records that the *entity*
	// pass's early return here was a leak and was removed; this one was left,
	// and it skips refsLeak — which the comment five lines down calls "the view
	// that survives a value decodeURLRefs declines". So a srcset holding a
	// fusing fragment, an origin the locator catches, and a reference-encoded
	// origin rewrote the first and served the second live, with the census
	// reporting a successful rewrite and zero skips. Chrome decodes `&#47;&#47;`
	// in that attribute and POSTs to it on click.
	cur = w.normaliseURLLeak(SurfaceHTMLObfuscated, base, cur, true)
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
	//
	// Recorded per splice, not once per value. This was the last path still
	// reporting a single synthetic event with no text — a `srcset` holding three
	// reference-encoded origins plus a fusing fragment counted 1 where every
	// other view now counts 3, and the one event it did emit named no bytes, so
	// --explain pointed at the start of the value rather than at any origin.
	if w.hosts != nil {
		if out := w.refsLeak(SurfaceHTMLEntity, base, cur, true); !bytes.Equal(out, cur) {
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
				// Whether the *element* is a foreign one is decided here, at its
				// start tag, and not later by whatever the stack says while its
				// content is being written. `<svg><foreignObject><title>` is an
				// HTML title — HTML rules resumed at the foreignObject — so a
				// `<style>` inside it is literal text and not an element, while
				// `<svg><title>` is an SVG one where the parser builds a real
				// stylesheet. Reading the current state instead gave the first a
				// CSS view, and the CSS view is the one view here that does not
				// decode references, so the decoded title text kept naming
				// production. §4.4's copy-paste hazard, found by the generated
				// grid and by neither auditor.
				w.rawTextForeign = w.inForeignContent()
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
			// An HTML tag from 13.2.6.5's breakout list ends foreign content
			// wherever it appears in it: the parser pops back to the nearest
			// integration point or HTML element and carries on under HTML rules.
			// So `<svg><p>` — an unclosed `<svg>` and any ordinary markup after
			// it, which is what a malformed inline icon looks like — leaves a
			// later `<script>` an HTML one, where references are not decoded.
			// Without this the model stayed foreign to the end of the document
			// and rewrote the value of a string no browser resolves. Over-decode,
			// so it never shipped a canonical origin, but it changed bytes that
			// had nothing to do with a URL.
			//
			// Outside the start-tag gate below, because the rule is a *name*
			// test and the self-closing flag is no part of it. `<br/>`, `<hr/>`,
			// `<img/>`, `<meta/>` and `<embed/>` are the ordinary spelling for
			// the void elements on that list, and round 63 gated all of this on
			// StartTagToken — so every one of them left the model foreign.
			if n := string(bytes.ToLower(tagNameOf(raw))); w.inForeignContent() &&
				breaksOutOfForeign(n, raw) {
				w.foreignNS = w.foreignNS[:w.foreignObjectAt]
			}
			// `<mglyph>` and `<malignmark>` are 13.2.6's carve-out: inside a
			// MathML text integration point every start tag goes to the HTML
			// insertion mode *except* those two, which are processed by the
			// foreign-content rules and inserted as MathML elements. So the
			// parser is back in foreign content below them and decodes character
			// references there — measured, `<math><mi><mglyph><script>` puts that
			// script in the MathML namespace with its references resolved, while
			// `<math><mi><span><script>` is an HTML script where they are not.
			//
			// Round 63's daily audit flagged this as unsettled and could not
			// build a payload; the payload is a `<script>` the page runs.
			//
			// Not restored on the end tag: staying foreign for the rest of the
			// integration point over-decodes, which is the direction to fail in.
			if w.foreignObjectAt > 0 && w.mathTextPoint &&
				len(w.foreignNS) == w.foreignObjectAt {
				switch string(bytes.ToLower(tagNameOf(raw))) {
				case "mglyph", "malignmark":
					w.foreignObjectAt = 0
				}
			}
			// The push stays start-tags-only: a self-closing `<svg/>` opens and
			// closes in the same token, so it never becomes the current node.
			if tt == html.StartTagToken {
				n := string(bytes.ToLower(tagNameOf(raw)))
				// The namespace decides, not the name.
				//
				// An HTML integration point is an *SVG* foreignObject/desc/
				// title; a MathML text integration point is a *MathML*
				// mi/mo/mn/ms/mtext. `<svg><mtext>` is an ordinary SVG element
				// with no special meaning, so the parser stays foreign and the
				// browser decodes references inside it — measured in Chrome,
				// which built a live stylesheet from an `@import` there and
				// issued the request. Round 61 matched on the name alone and
				// withheld the decode, shipping the canonical origin.
				//
				// A stack and not a counter, for the reason the previous round's
				// title gives: the innermost vocabulary is the question, and a
				// count cannot answer it.
				switch n {
				case "svg", "math":
					// In foreign content the namespace comes from the adjusted
					// current node, not the tag name — `<svg><math>` is an SVG
					// element. Only where HTML rules are in force does `<math>`
					// open MathML. Reading the name as the vocabulary made every
					// interleaved subtree the wrong one.
					ns := n
					if w.inForeignContent() {
						ns = w.currentNS()
					}
					w.foreignNS = append(w.foreignNS, foreignEl{name: n, ns: ns})
				case "annotation-xml":
					// An integration point only when its encoding is an HTML
					// one (13.2.6). Without that it is ordinary MathML and the
					// parser stays in foreign content, so admitting it
					// unconditionally withheld a decode the browser performs.
					if w.currentNS() == "math" && htmlEncoding(raw) &&
						w.foreignObjectAt == 0 {
						w.foreignObjectAt = len(w.foreignNS)
					}
				case "foreignobject", "desc", "title",
					"mi", "mo", "mn", "ms", "mtext":
					// Every HTML integration point, not just the one named for
					// it. SVG has three (`foreignObject`, `desc`, `title`) and
					// MathML has five text integration points (`mi`, `mo`,
					// `mn`, `ms`, `mtext`) plus `annotation-xml` when its
					// encoding is an HTML one. In all of them the parser is
					// back on HTML rules and decodes nothing in a `<script>`,
					// and modelling one of seven is what made that one a
					// special case rather than a rule.
					//
					//
					// Only the outermost is recorded; a nested pair resolves to
					// "foreign", which over-decodes.
					if integrationPointIn(n, w.currentNS()) && w.foreignObjectAt == 0 {
						w.foreignObjectAt = len(w.foreignNS)
						// Only a MathML *text* integration point carries the
						// mglyph/malignmark carve-out; SVG has no analogue.
						w.mathTextPoint = w.currentNS() == "math"
					}
				}
			}
			w.write(off, len(raw), w.rewriteTag(raw, off))
		case html.EndTagToken:
			w.rawText = ""
			switch n := string(bytes.ToLower(endTagNameOf(raw))); n {
			case "svg", "math":
				w.popForeign(n)
			case "foreignobject", "desc", "title",
				"mi", "mo", "mn", "ms", "mtext", "annotation-xml":
				w.foreignObjectAt = 0
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
				// A `<style>` the tokenizer swallowed into a raw-text token.
				//
				// `<title>` is a raw-text element by name and the tokenizer is
				// context-free — but inside `<svg>` it is an HTML integration
				// point, so the parser builds a *real* `<style>` element in it
				// and runs its CSS tokenizer. (`desc` is the other SVG
				// integration point that could hold one, and it is not in
				// rawTextNames, so it never reaches here.) hostshift saw one
				// token named `title` and withheld the CSS view, so
				// `<svg><title><style>@import url(https\3a \2f \2f canonical/x)`
				// went out untouched — an `@import` the browser fetches, which
				// is test 28 with an authenticated request behind it.
				//
				// Gated on the token actually containing a `<style`: running the
				// CSS decode over every title would over-rewrite bytes a reader
				// sees, which is the defect round 60 fixed for text/plain.
				// …and only over the `<style>` element itself. Switching the
				// whole token to CSS put the escape view over the text beside
				// it, where `https\3a \2f \2f canonical/x` is bytes a reader
				// sees and not a stylesheet — round 60's text/plain over-rewrite
				// again, in one element. The text keeps the raw-text view, which
				// still decodes character references in foreign content.
				if w.rawTextForeign && integrationPointIn(w.rawText, w.currentNS()) &&
					tagStartIn(bytes.ToLower(raw), "style") >= 0 {
					w.writeRawTextAroundStyles(off, raw)
					break
				}
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
