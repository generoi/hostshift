package rewrite

import (
	"bytes"
	"io"

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
// The matcher runs over the *raw*, still-escaped bytes of each string, which is
// why HTML nested inside a JSON string needs no special handling: the origins in
// content.rendered appear literally as https:\/\/host\/… and the automaton
// already carries the JSON-escaped form. Decoding the value, running the HTML
// rewriter over it and re-encoding would be strictly worse — re-encoding is
// re-serialisation, and §5.2's core property is that output is byte-identical
// everywhere a rewrite did not occur.
//
// JSON is buffered rather than streamed (PLAN §5.8); the caller applies the size
// cap and passes through untouched above it.
//
// Malformed JSON is returned unchanged rather than half-rewritten: whatever the
// upstream meant by it, corrupting it is worse than leaving it.
func RewriteJSON(b []byte, m *origin.Matcher, st *Stats, explain bool) []byte {
	if len(b) == 0 {
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
				return b
			}
			break
		}

		top := len(stack) - 1
		inObject := top >= 0 && stack[top].object

		switch kind {
		case '{', '[':
			if _, err := d.ReadToken(); err != nil {
				return b
			}
			stack = append(stack, frame{object: kind == '{', wantKey: kind == '{'})
			continue

		case '}', ']':
			if _, err := d.ReadToken(); err != nil {
				return b
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
			return b
		}
		end := int(d.InputOffset())
		start := end - len(v)
		if start < 0 || end > len(b) {
			return b // the span contract did not hold; do not splice blind
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
		}
		st.Record(SurfaceJSONString, start, ev)
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
		return b // an unterminated object or array: the document was truncated
	}
	if out == nil {
		return b // untouched: the same bytes, not a copy
	}
	return append(out, b[last:]...)
}
