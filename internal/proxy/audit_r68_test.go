package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A boundary Go's media-type parser refuses and PHP's accepts.
//
// The two readers do not share an alphabet. RFC 2046 §5.1.1 defines the
// boundary's own alphabet as `bchars`:
//
//	DIGIT / ALPHA / "'" / "(" / ")" / "+" / "_" / "," / "-" / "." /
//	"/" / ":" / "=" / "?" / " "
//
// Seven of those — "(", ")", ",", "/", ":", "=", " " — are RFC 2045 tspecials
// rather than token characters, so a boundary containing one has to be quoted
// to survive a strict parameter parse. Producers routinely do not quote it:
// `----=_Part_0_12345.67890` is the JavaMail/Apache Commons shape, and any
// base64-derived boundary carries `=` padding. `mime.ParseMediaType` then
// returns ErrInvalidMediaParameter and a nil params map, and rewriteMultipart's
// first three lines return the body *unchanged*:
//
//	internal/proxy/multipart.go:21-27
//	    _, params, err := mime.ParseMediaType(ct)
//	    if err != nil {
//	        return body
//	    }
//
// PHP's own parser does not tokenise at all. `php_rfc1867.c` locates the
// boundary with `strstr(content_type, "boundary")`, steps to the next `=`, and
// takes everything up to the next `,` or `;`, so it reads every boundary below
// exactly and parses the parts. The form field is therefore stored — with the
// worktree's hostname in it. That is PLAN §4.3: the shared database is no
// longer byte-identical to production, and the write is unrecoverable.
//
// The boundary alphabet is only the cheapest way in. The defect is the arm: any
// Content-Type `mime.ParseMediaType` rejects for any reason — a duplicate
// parameter, a stray token after the boundary, an unclosed quoted string — takes
// the same silent `return body`, and so does a body whose delimiters this
// function cannot find.
//
// The failure is silent in every channel hostshift has:
//
//   - no log line — the `err` is discarded, not reported;
//   - no straggler sweep — `bodyMultipart` is the one arm of rewriteRequestBody
//     with no `HostLeaksBack` behind it (internal/proxy/proxy.go:786-790);
//   - no counter — nothing calls `Stats.Record`, so `--json` shows zero
//     candidates rather than a skip;
//   - `hostshift diff` never looks at a request at all.
//
// Contrast the sibling arms, which all fail loudly: `bodyJSON` logs a WARN and
// falls back to SweepBytes when jsontext rejects the document, and the
// over-cap path logs and records origin.ReasonSizeCap. Multipart is the arm
// that says nothing.
func TestR68AMultipartBoundaryGoRefusesLetsAVariantHostReachTheDatabase(t *testing.T) {
	// The value a page-builder field holds after the response direction served
	// it: the variant hostname, which must be mapped back before it is stored.
	field := `<div style="background:url(https://` + variantHost + `/a.png)"></div>`

	for _, c := range []struct{ name, boundary string }{
		// Every one of these is bchars per RFC 2046 §5.1.1, and every one of
		// them is read whole by php_rfc1867.c — none contains a "," or ";",
		// which are the only two bytes PHP stops at.
		{"javamail", "----=_Part_0_12345.67890"},
		{"base64 padded", "Ck6Lz+Kk1Q=="},
		{"equals run", "===============1234567890=="},
		{"colon", "a:b"},
		{"slash", "a/b"},
		{"parens", "a(b)c"},
		{"space", "b1 b2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ct := "multipart/form-data; boundary=" + c.boundary
			body := "--" + c.boundary + "\r\n" +
				"Content-Disposition: form-data; name=\"content\"\r\n\r\n" +
				field + "\r\n" +
				"--" + c.boundary + "--\r\n"

			h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			})
			res, rb := h.do(t, "POST", variantHost, "/wp-admin/admin-ajax.php", ct, []byte(body))
			if h.seen == nil {
				t.Fatalf("the upstream was never reached: status %d, body %q", res.StatusCode, rb)
			}
			up, err := io.ReadAll(h.seen.Body)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(up, []byte(variantHost)) {
				t.Errorf("a variant hostname reached the upstream in a multipart body whose "+
					"boundary is RFC 2046-conforming but not an RFC 2045 token (PLAN §4.3)\n"+
					"  content-type %q\n  sent      %q\n  upstream  %q",
					ct, body, up)
			}
			if strings.Contains(string(up), canonical) {
				return // mapped back, which is the correct outcome
			}
		})
	}
}

// The same body with a token-safe boundary is mapped back, which is what makes
// the case above a defect in the boundary reader rather than in the rewriter.
func TestR68TheSameBodyWithATokenBoundaryIsMappedBack(t *testing.T) {
	field := `<div style="background:url(https://` + variantHost + `/a.png)"></div>`
	ct := "multipart/form-data; boundary=BXX"
	body := "--BXX\r\nContent-Disposition: form-data; name=\"content\"\r\n\r\n" +
		field + "\r\n--BXX--\r\n"

	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	if _, _ = h.do(t, "POST", variantHost, "/wp-admin/admin-ajax.php", ct, []byte(body)); h.seen == nil {
		t.Fatal("upstream never reached")
	}
	up, _ := io.ReadAll(h.seen.Body)
	if bytes.Contains(up, []byte(variantHost)) {
		t.Fatalf("control case regressed: %q", up)
	}
}
