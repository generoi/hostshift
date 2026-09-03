package proxy

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

// Round 66: one media type, read two ways in the same function.
//
// A media type is case-insensitive (RFC 9110 §8.3.1), and every other place in
// this project reads it that way: bodyKind lowercases (proxy.go:981), and so
// does the filter's dispatch (cmd/hostshift/main.go:255). The one site that does
// not is the choice of repair inside rewriteRequestBody:
//
//	if mediaType(r.Header.Get("Content-Type")) == "application/x-www-form-urlencoded" {
//	        repair = rewrite.RepairSerializedFields
//	}
//
// So a body whose Content-Type carries any upper-case letter is still classified
// bodyFlat by bodyKind — it *is* rewritten — but is repaired with the
// non-splitting RepairSerialized. That drops two things at once, and both have
// their own §4.3 finding in this project's history:
//
//   - the field split, so one option holding `a:hover{color:red}` leaves every
//     other option in the same `options.php` POST with a stale length;
//   - peelFormField, so the double-encoded spelling a browser posts back —
//     `https%253A%252F%252F<variant>` — is reachable by no spelling in the
//     table, and the *variant* hostname is written into the shared database.
//
// The second is round 63's finding, read back out of a real database, and this
// re-opens it for any sender that spells the header differently. `check` and
// `diff` cannot see it: neither makes a request-direction assertion.
func TestAFormBodyIsSplitWhateverTheHeadersCase(t *testing.T) {
	// The Customizer's `customized` field, in the spelling only the peel reaches.
	const body = "customized=https%253A%252F%252F" + variantHost + "%252Fa"

	for _, ct := range []string{
		"application/x-www-form-urlencoded",
		"application/x-www-form-urlencoded; charset=UTF-8",
		"Application/X-WWW-Form-Urlencoded",
		"APPLICATION/X-WWW-FORM-URLENCODED",
		"application/X-Www-Form-Urlencoded; charset=utf-8",
	} {
		t.Run(ct, func(t *testing.T) {
			h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			})
			h.do(t, "POST", variantHost, "/wp-admin/admin-ajax.php", ct, []byte(body))
			if h.seen == nil {
				t.Fatal("the upstream was never reached")
			}
			up, _ := io.ReadAll(h.seen.Body)
			if bytes.Contains(up, []byte(variantHost)) {
				t.Errorf("a variant hostname reached the upstream, so it would be "+
					"written into the shared database (PLAN §4.3):\n sent %s\n up   %s",
					body, up)
			}
		})
	}
}
