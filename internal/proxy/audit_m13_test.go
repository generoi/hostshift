package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"testing"
)

// RepairSerializedFields runs its whole-buffer fallback only when *no* field
// carried a payload, so one serialized option disarms it for the rest of the
// body.
//
// The fallback exists because the field split is on a raw `&`, and a character
// reference is written with one too: `https:&#47;&#47;host` is cut into
// `https:`, `#47;` and `#47;host`, and the host is in none of them as far as the
// byte matcher is concerned — it needs `//`, `\/` or `%2F` in front of a host.
// RepairSerializedFields' own comment says so, and says that dropping the
// fallback "sent a variant hostname to the upstream in two of the request-body
// spellings".
//
// It is dropped, whenever anything else in the same body is serialized. That is
// not a corner: `options.php` posts every option on a settings page in one body,
// which is the scenario the field split was introduced for, and an editor posts
// content back with the reference-encoded origins the forward direction spliced
// into it. The two conditions are the same POST.
//
// So the variant hostname goes upstream and into the shared database — the one
// failure §4.3 says the whole design exists to prevent — and it does so only
// when a second field makes it invisible, which is why the existing
// TestEveryRequestBodyArmReadsEverySpelling cannot see it: its urlencoded body
// is a single `content=` field.
func TestOneSerializedFieldDoesNotDisarmTheWholeBufferPass(t *testing.T) {
	// A serialized option, its lengths computed by the same helper the rest of
	// the suite uses, percent-encoded the way a form body carries one.
	blob := `a:1:{s:1:"k";s:1:"v";}`
	reference := "https:&#47;&#47;" + variantHost + "/a.png"

	for _, tc := range []struct{ name, body string }{
		// The shape the existing suite already covers, kept as the control: with
		// no payload anywhere the fallback fires and the host is mapped back.
		{"one field", "content=" + reference},
		// The same value, in a body that also carries a serialized option.
		{"beside a serialized option", "option=" + url.QueryEscape(blob) +
			"&content=" + reference},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			})
			res, rb := h.do(t, "POST", variantHost, "/wp-admin/options.php",
				"application/x-www-form-urlencoded", []byte(tc.body))
			if h.seen == nil {
				t.Fatalf("the upstream was never reached: status %d, body %q", res.StatusCode, rb)
			}
			up, _ := io.ReadAll(h.seen.Body)
			if bytes.Contains(up, []byte(variantHost)) {
				t.Errorf("a variant hostname reached the upstream, so it would be "+
					"written into the shared database:\nsent %s\nup   %s", tc.body, up)
			}
		})
	}
}
