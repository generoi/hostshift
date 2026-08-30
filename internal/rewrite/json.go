package rewrite

import (
	"bytes"
	"io"
	"log/slog"

	"github.com/go-json-experiment/json/jsontext"

	"github.com/generoi/hostshift/internal/origin"
)

// RewriteJSON rewrites origins in JSON string *values*, by locating their spans
// and splicing — never by decoding and re-encoding.
//
// jsontext supplies the spans: ReadValue returns "the exact bytes of the input"
// and InputOffset "the location of the next byte immediately after the most
// recently returned value", so start = end - len(v) holds by construction, and
// StackPointer gives an RFC 6901 path for --explain. It is used through
// github.com/go-json-experiment/json rather than the standard library's
// encoding/json/jsontext, which on Go 1.26.5 still needs GOEXPERIMENT=jsonv2 — a
// plain import fails with "build constraints exclude all Go files", and
// requiring an environment variable to build a distributed binary is not a
// trade worth making. It is the same code (PLAN §5.7).
//
// The matcher runs over the *raw*, still-escaped bytes of each string, because
// the origins in content.rendered usually appear literally as https:\/\/host\/…
// and the automaton already carries the JSON-escaped form. Where that is not
// true — an escape spelling the raw scan cannot see — decodeJSONLeak below is
// the enumerated carve-out.
//
// JSON is buffered rather than streamed (PLAN §5.8); the caller applies the size
// cap and passes through untouched above it.
//
// Malformed JSON is returned unchanged rather than half-rewritten: whatever the
// upstream meant by it, corrupting it is worse than leaving it. That is a
// *reported* outcome, not a silent one — see the ReasonNotDecodable path.
func RewriteJSON(b []byte, m *origin.Matcher, st *Stats, log *slog.Logger, explain bool) []byte {
	if len(b) == 0 {
		return b
	}
	if log == nil {
		log = slog.Default()
	}

	// Events are held back until the document parses.
	//
	// They used to be folded into Stats as each value was scanned, so a decoder
	// error part way through returned the original bytes while the counters
	// kept the rewrites that had been undone. A duplicate object member —
	// legal JSON, rejected by jsontext's default — printed two production
	// origins on stdout under "rewrites: {json-string: 1}", "skips: {}" and
	// exit 0. The census has to be able to say a body was skipped.
	var pending, pendingEsc []origin.Event
	fail := func(reason string) []byte {
		log.Warn("JSON body could not be parsed, passing through untouched",
			"reason", reason, "bytes", len(b))
		st.Record(SurfaceJSONString, 0, []origin.Event{{
			Surface: SurfaceJSONString, Action: origin.ActionSkipped,
			Reason: origin.ReasonNotDecodable,
		}})
		return b
	}
	type frame struct {
		object  bool
		wantKey bool
	}

	d := jsontext.NewDecoder(bytes.NewReader(b))
	var (
		stack []frame
		out   []byte
		last  int
	)

	for {
		kind := d.PeekKind()
		if kind == 0 {
			// Either the end of the document or a syntax error. Only a clean
			// io.EOF is acceptable: anything else means the input was not the
			// JSON it claimed to be, and a half-rewritten body is worse than an
			// unrewritten one.
			if _, err := d.ReadToken(); err != io.EOF {
				return fail("syntax")
			}
			break
		}

		top := len(stack) - 1
		inObject := top >= 0 && stack[top].object

		switch kind {
		case '{', '[':
			if _, err := d.ReadToken(); err != nil {
				return fail("syntax")
			}
			stack = append(stack, frame{object: kind == '{', wantKey: kind == '{'})
			continue

		case '}', ']':
			if _, err := d.ReadToken(); err != nil {
				return fail("syntax")
			}
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			if t := len(stack) - 1; t >= 0 && stack[t].object {
				stack[t].wantKey = true // the member this container was is complete
			}
			continue
		}

		isKey := inObject && stack[top].wantKey
		ptr := string(d.StackPointer())

		v, err := d.ReadValue()
		if err != nil {
			return fail("syntax")
		}
		end := int(d.InputOffset())
		start := end - len(v)
		if start < 0 || end > len(b) {
			return fail("span") // the span contract did not hold; do not splice blind
		}

		if inObject {
			stack[top].wantKey = !isKey // a key is followed by a value, and vice versa
		}

		// Keys are left alone. An origin in a JSON key is not a URL the browser
		// will ever dereference, and rewriting one would change the shape of the
		// document rather than its links.
		if isKey || kind != '"' {
			continue
		}

		nv, ev := m.Rewrite(v, SurfaceJSONString, explain)
		for i := range ev {
			ev[i].Path = ptr
			ev[i].Offset += start
		}
		pending = append(pending, ev...)

		if dv, ok := decodeJSONLeak(m, nv); ok {
			pendingEsc = append(pendingEsc, origin.Event{
				Surface: SurfaceJSONEscape, Action: origin.ActionRewrote,
				Offset: start, Path: ptr, Text: string(v),
			})
			nv = dv
		}
		if bytes.Equal(nv, v) {
			continue
		}
		if out == nil {
			out = make([]byte, 0, len(b)+len(b)/8)
		}
		out = append(out, b[last:start]...)
		out = append(out, nv...)
		last = end
	}

	if len(stack) != 0 {
		return fail("truncated") // an unterminated object or array
	}
	st.Record(SurfaceJSONString, 0, pending)
	st.Record(SurfaceJSONEscape, 0, pendingEsc)
	if out == nil {
		return b // untouched: the same bytes, not a copy
	}
	return append(out, b[last:]...)
}

// decodeJSONLeak is the JSON counterpart of §5.3's entity carve-out: the escape
// spellings a raw scan over the still-quoted bytes cannot see.
//
// The raw scan is right for the common case — WordPress renders origins into
// JSON as https:\/\/host\/…, which the automaton carries — but three real
// spellings defeat it, and each one puts a dereferenceable production origin in
// front of the browser (test 28):
//
//   - \uXXXX. PHP's json_encode escapes every non-ASCII rune unless
//     JSON_UNESCAPED_UNICODE is passed, and wp_json_encode does not pass it. So
//     an IDN client site — §5.5 calls those "real for .fi client domains" —
//     stores https://hämeen.fi and serves "https:\/\/hämeen.fi\/x". The
//     page rewrites; the REST API does not, so Gutenberg and every JS fetch get
//     production URLs.
//   - HTML character references inside content.rendered, the same class M3
//     closed for attribute values. The identical post body is clean as
//     text/html and leaks as application/json.
//   - Double-escaped JSON-in-JSON, "https:\\/\\/host", which appears when a
//     block attribute holding JSON is itself serialised into JSON.
//
// The value is unquoted, entity-decoded and re-matched. Only when that finds an
// origin the raw pass did not is the string re-encoded and spliced — so a
// document that is already correct never takes this path, and byte-identity
// under an identity map is untouched. Re-encoding one string value is the
// re-serialisation §5.2 forbids in general; it is confined to strings that would
// otherwise leak, and it is counted under its own surface.
func decodeJSONLeak(m *origin.Matcher, v []byte) ([]byte, bool) {
	dec, err := jsontext.AppendUnquote(nil, v)
	if err != nil {
		return nil, false // not a string, or not decodable: leave it alone
	}
	dec, _ = decodeURLRefs(dec)

	out, _ := m.Rewrite(dec, SurfaceJSONEscape, false)
	// The same two catchers the HTML surfaces get. Without them the REST body
	// was the one surface with neither: `{"u":"https:\\h/x"}` and an NFD host in
	// content.rendered both went out untouched while the identical bytes in the
	// page were rewritten — which is the hazard this function's own header
	// describes, "the page rewrites; the REST API does not, so Gutenberg and
	// every JS fetch get production URLs". And because this is also the
	// request-body path, a Gutenberg save wrote the unfolded host back into the
	// database.
	//
	// value=true: a JSON string holding a URL is a value, so a trailing dot is
	// the host's root label rather than a sentence's full stop.
	out = hostsFor(m).rewriteAll(out, true)
	if bytes.Equal(out, dec) {
		return nil, false
	}
	q, err := jsontext.AppendQuote(nil, out)
	if err != nil {
		return nil, false // invalid UTF-8: passing it through beats corrupting it
	}
	return q, true
}
