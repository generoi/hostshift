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

	// Delimiter positions: at the very start, or preceded by a line ending.
	//
	// A bare LF counts. RFC 2046 requires CRLF and every browser sends it, but
	// bodies assembled by hand — a PHP client building the parts as a string, a
	// JS test fixture, curl reading a file that has been through a text editor
	// — routinely use LF, and requiring CRLF meant those bodies matched no
	// delimiter at all and passed through with their variant origins intact.
	// The write then stores dev hostnames in the database, which is what §5.1's
	// request direction exists to prevent (tests 30 and 31).
	//
	// The leading line ending is recorded per delimiter rather than assumed for
	// the whole body, because it is what the previous part's content has to
	// stop short of, and a hand-built body can mix the two.
	var starts []delimiter
	for i := 0; ; {
		j := bytes.Index(body[i:], delim)
		if j < 0 {
			break
		}
		at := i + j
		switch {
		case at == 0:
			starts = append(starts, delimiter{at: at})
		case at >= 2 && body[at-2] == '\r' && body[at-1] == '\n':
			starts = append(starts, delimiter{at: at, eol: 2})
		case at >= 1 && body[at-1] == '\n':
			starts = append(starts, delimiter{at: at, eol: 1})
		}
		i = at + len(delim)
	}
	if len(starts) < 2 {
		return body
	}

	var out []byte
	last := 0
	for k := 0; k+1 < len(starts); k++ {
		// Part content runs from just after this delimiter's trailing line
		// ending to just before the next delimiter's leading one.
		p := starts[k].at + len(delim)
		n := eolAt(body, p)
		if n == 0 {
			continue // closing delimiter ("--BOUNDARY--") or malformed
		}
		p += n
		end := starts[k+1].at - starts[k+1].eol
		if end < p {
			continue
		}

		hdrEnd, sep := headerEnd(body[p:end])
		if hdrEnd < 0 {
			continue
		}
		headers := body[p : p+hdrEnd]
		bodyStart := p + hdrEnd + sep
		if !rewritablePart(headers) {
			continue
		}

		// Through RepairSerialized for the same reason the flat arm is: a part
		// can carry a PHP-serialized blob, and a stale length prefix makes PHP
		// refuse the whole structure.
		var ev []origin.Event
		nv := rewrite.RepairSerialized(body[bodyStart:end], func(b []byte) []byte {
			out, nev := m.Rewrite(b, rewrite.SurfaceRequestBody, explain)
			ev = append(ev, nev...)
			return out
		})
		// HostLeaksBack, not HostLeaks: this is a *request* body, and the two
		// directions are not symmetric surfaces. HostLeaks has no reference view
		// and no composed refs→CSS view, so a part carrying
		// `style="background:url(https&#92;3a &#92;2f &#92;2f<variant>/a.png)"` —
		// which is exactly what the forward pass emits into a style attribute —
		// went upstream with the variant hostname in it and into the shared
		// database. Every other request-direction call site was moved; this one
		// was missed, and a multipart POST is what any form with a file field
		// sends: the media library, an editor with an attachment, Gravity Forms.
		nv = rewrite.RepairSerialized(nv, func(b []byte) []byte {
			return rewrite.HostLeaksBackCounted(m, b, st, rewrite.SurfaceRequestBody, bodyStart)
		})
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

// delimiter is one boundary occurrence and the length of the line ending
// immediately before it — 0 at the very start of the body.
type delimiter struct{ at, eol int }

// eolAt returns the length of the line ending at i: 2 for CRLF, 1 for a bare
// LF, 0 for neither.
func eolAt(b []byte, i int) int {
	if i+1 < len(b) && b[i] == '\r' && b[i+1] == '\n' {
		return 2
	}
	if i < len(b) && b[i] == '\n' {
		return 1
	}
	return 0
}

// headerEnd finds the blank line separating a part's headers from its body,
// returning its offset and the length of the separator. Whichever spelling
// comes first wins, so a body that mixes them still splits where the reader on
// the other end will split it.
func headerEnd(b []byte) (int, int) {
	crlf := bytes.Index(b, []byte("\r\n\r\n"))
	lf := bytes.Index(b, []byte("\n\n"))
	switch {
	case crlf >= 0 && (lf < 0 || crlf <= lf):
		return crlf, 4
	case lf >= 0:
		return lf, 2
	}
	return -1, 0
}

// rewritablePart reports whether a part's body may be rewritten: only parts
// whose Content-Disposition carries no filename= and whose type is text (or
// absent, which is the norm for a plain form field).
func rewritablePart(headers []byte) bool {
	var disposition, ctype string
	// Split on LF and drop a trailing CR, so a header block written with either
	// line ending parses the same way.
	for _, line := range strings.Split(string(headers), "\n") {
		line = strings.TrimSuffix(line, "\r")
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
		// testing the value let an empty file input through. ParseMediaType
		// folds RFC 2231's filename*= into "filename" itself.
		if _, isFile := params["filename"]; isFile {
			return false
		}
	} else {
		// The parse failed, so this is a hand-rolled scan over a disposition
		// that is already malformed — which is exactly when it must be
		// pessimistic. "filename=" alone missed RFC 2231's extended form,
		// `filename*=UTF-8''photo.jpg`, so a file part with a malformed
		// disposition had its bytes rewritten.
		lower := strings.ToLower(disposition)
		if strings.Contains(lower, "filename=") || strings.Contains(lower, "filename*=") {
			return false
		}
	}
	if ctype == "" {
		return true
	}
	mt := strings.ToLower(mediaType(strings.TrimSpace(ctype)))
	return strings.HasPrefix(mt, "text/") || mt == "application/json" || strings.HasSuffix(mt, "+json")
}
