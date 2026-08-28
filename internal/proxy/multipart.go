package proxy

import (
	"bytes"
	"mime"
	"strings"

	"github.com/generoi/hostshift/internal/origin"
	"github.com/generoi/hostshift/internal/rewrite"
)

// rewriteMultipart rewrites only the bodies of non-file text parts, splicing
// them in place (PLAN §5.1).
//
// It deliberately does not use mime/multipart's Reader and Writer. Re-emitting
// a body through Writer would rebuild every boundary and header, so an upload
// that hostshift did not need to touch at all would come out with different
// bytes — the same class of silent divergence that ruled out lol-html in §5.7.
// Locating spans and splicing keeps file parts, boundaries and headers
// byte-identical, which is what "file parts pass through byte-identical" means.
func rewriteMultipart(body []byte, ct string, m *origin.Matcher, st *rewrite.Stats, explain bool) []byte {
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return body
	}
	boundary := params["boundary"]
	if boundary == "" {
		return body
	}
	delim := []byte("--" + boundary)

	// Delimiter positions: at the very start, or preceded by CRLF.
	var starts []int
	for i := 0; ; {
		j := bytes.Index(body[i:], delim)
		if j < 0 {
			break
		}
		at := i + j
		if at == 0 || (at >= 2 && body[at-2] == '\r' && body[at-1] == '\n') {
			starts = append(starts, at)
		}
		i = at + len(delim)
	}
	if len(starts) < 2 {
		return body
	}

	var out []byte
	last := 0
	for k := 0; k+1 < len(starts); k++ {
		// Part content runs from just after this delimiter's trailing CRLF to
		// just before the next delimiter's leading CRLF.
		p := starts[k] + len(delim)
		if p+1 >= len(body) || body[p] != '\r' || body[p+1] != '\n' {
			continue // closing delimiter ("--BOUNDARY--") or malformed
		}
		p += 2
		end := starts[k+1] - 2 // strip the CRLF that belongs to the next delimiter
		if end < p {
			continue
		}

		hdrEnd := bytes.Index(body[p:end], []byte("\r\n\r\n"))
		if hdrEnd < 0 {
			continue
		}
		headers := body[p : p+hdrEnd]
		bodyStart := p + hdrEnd + 4
		if !rewritablePart(headers) {
			continue
		}

		nv, ev := m.Rewrite(body[bodyStart:end], rewrite.SurfaceRequestBody, explain)
		st.Record(rewrite.SurfaceRequestBody, bodyStart, ev)
		if bytes.Equal(nv, body[bodyStart:end]) {
			continue
		}
		if out == nil {
			out = make([]byte, 0, len(body)+len(body)/8)
		}
		out = append(out, body[last:bodyStart]...)
		out = append(out, nv...)
		last = end
	}
	if out == nil {
		return body // untouched: the same bytes, not a copy
	}
	return append(out, body[last:]...)
}

// rewritablePart reports whether a part's body may be rewritten: only parts
// whose Content-Disposition carries no filename= and whose type is text (or
// absent, which is the norm for a plain form field).
func rewritablePart(headers []byte) bool {
	var disposition, ctype string
	for _, line := range strings.Split(string(headers), "\r\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "content-disposition":
			disposition = v
		case "content-type":
			ctype = v
		}
	}
	// A part with no Content-Disposition at all is not a form field — §5.1
	// scopes rewriting to parts *whose Content-Disposition carries no
	// filename=*, which presupposes there is one. Rewriting an unlabelled part
	// was reading the rule as its converse.
	if strings.TrimSpace(disposition) == "" {
		return false
	}
	if _, params, err := mime.ParseMediaType(strings.TrimSpace(disposition)); err == nil {
		// Presence, not non-emptiness. filename="" still means a file part;
		// testing the value let an empty file input through.
		if _, isFile := params["filename"]; isFile {
			return false
		}
	} else if strings.Contains(strings.ToLower(disposition), "filename=") {
		return false
	}
	if ctype == "" {
		return true
	}
	mt := strings.ToLower(mediaType(strings.TrimSpace(ctype)))
	return strings.HasPrefix(mt, "text/") || mt == "application/json" || strings.HasSuffix(mt, "+json")
}
