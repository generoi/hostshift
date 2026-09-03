package proxy

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// Round 67, on 5cb050f, auditing round 66's own new code: HiddenInBase64.
//
// PLAN §4.3 accepts that a variant hostname inside a base64 blob cannot be
// mapped back — the Customizer validates a widget instance with `wp_hash()`
// over exactly those bytes — and says the failure was never the rewrite but the
// silence: "the save went through, production's database took the worktree's
// hostname, the canonical front page served it to the public, nothing logged a
// line, and `hostshift diff` printed GREEN. `HiddenInBase64` reports it instead,
// on both arms".
//
// It reports it on one arm, for one spelling, and that spelling is not the one
// any client sends.
//
//   - proxy.go:849 calls it only from the `default:` (flat) arm of
//     rewriteRequestBody's switch. bodyJSON and bodyMultipart never call it —
//     so a widget saved through `POST /wp-json/wp/v2/widgets`, or any form with
//     a file field beside it, is silent by construction.
//
//   - base64.go:32 walks runs of `[A-Za-z0-9+/]` and requires
//     `base64.StdEncoding.DecodeString` to succeed over the whole run. In a
//     form body the blob is percent-encoded — `+` is `%2B`, `/` is `%2F` and
//     the padding is `%3D` — so the run is cut at every escape and each
//     fragment is unaligned, decodes to garbage, or fails outright. The escape's
//     own hex digits are `[A-Za-z0-9]`, so `%22` before a blob glues `22` onto
//     the front of the run and shifts its alignment by two even when the blob
//     itself is escape-free.
//
// base64_test.go's fixtures are `"customized=" + base64.StdEncoding.Encode(…)`
// spliced raw into a form body. That is the one spelling that works. A `<form>`
// POST, `URLSearchParams`, jQuery and `wp.customize` all percent-encode the
// field, and PLAN §4.3 has a whole paragraph on there being no single
// urlencoded encoder — the alphabet lesson was learned for the peel and not for
// the detector beside it.
//
// The other half of the claim fails with it: `WriteBacks` (diff.go:369) is
// `countLeaks` over the canonical response, and countLeaks runs the rewrite
// pipeline, which has no base64 view. So "it is what turns that Customizer run
// red" is false for the case it names — `hostshift diff` still prints GREEN.

// r67Blob is a widget instance carrying a variant hostname, in the shape
// `encoded_serialized_instance` really has: base64 of a PHP-serialized array.
func r67Blob() string {
	link := `<a href="https://` + variantHost + `/promo/">the promo</a>`
	s := `a:1:{s:7:"content";s:` + strconv.Itoa(len(link)) + `:"` + link + `";}`
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// r67Post sends one write through the proxy and returns what the upstream saw
// and everything the proxy logged while doing it.
func r67Post(t *testing.T, ct string, body []byte) (up []byte, logged string) {
	t.Helper()
	var lb bytes.Buffer
	h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}, func(p *Proxy) {
		p.Log = slog.New(slog.NewTextHandler(&lb, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	h.do(t, "POST", variantHost, "/wp-admin/admin-ajax.php", ct, body)
	if h.seen == nil {
		t.Fatal("the upstream was never reached")
	}
	up, _ = io.ReadAll(h.seen.Body)
	return up, lb.String()
}

func TestABase64WriteBackIsReportedInEverySpelling(t *testing.T) {
	blob := r67Blob()

	multi := func() (string, []byte) {
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		fw, err := w.CreateFormField("customized")
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(fw, blob)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return w.FormDataContentType(), b.Bytes()
	}
	mpCT, mpBody := multi()

	for _, c := range []struct {
		name, ct string
		body     []byte
	}{
		// The control, and the only spelling base64_test.go exercises.
		{"a flat body with the blob spliced in raw",
			"application/x-www-form-urlencoded",
			[]byte("action=customize_save&customized=" + blob)},

		// What every form encoder actually puts on the wire.
		{"the same blob percent-encoded, as a form encoder sends it",
			"application/x-www-form-urlencoded",
			[]byte("action=customize_save&customized=" + url.QueryEscape(blob))},

		// customize_save's real shape: every setting in one `customized` field,
		// JSON inside it, the blob inside a JSON string.
		{"the customize_save wire shape",
			"application/x-www-form-urlencoded",
			[]byte("action=customize_save&nonce=abc&customized=" + url.QueryEscape(
				`{"widget_text[2]":{"encoded_serialized_instance":"`+blob+
					`","instance_hash_key":"d41d8cd98f00b204e9800998ecf8427e"}}`))},

		// The REST route the block editor's widget save uses. The JSON arm of
		// rewriteRequestBody never calls HiddenInBase64 at all.
		{"a JSON request body",
			"application/json",
			[]byte(`{"instance":{"encoded":"` + blob + `"}}`)},

		// Any form with a file field beside the settings. The multipart arm
		// never calls it either.
		{"a multipart request body", mpCT, mpBody},
	} {
		t.Run(c.name, func(t *testing.T) {
			up, logged := r67Post(t, c.ct, c.body)
			if !bytes.Equal(up, c.body) {
				t.Fatalf("fixture broken: the proxy changed this body, so the blob is "+
					"not what reaches the database\n sent %s\n up   %s", c.body, up)
			}
			if !strings.Contains(logged, "base64") {
				t.Errorf("a variant hostname reached the shared database inside base64 "+
					"and nothing said so.\n"+
					"  PLAN §4.3: \"the failure was the silence … HiddenInBase64 reports it "+
					"instead, on both arms\".\n"+
					"  content-type: %s\n  body:         %.160s\n  logged:       %q",
					c.ct, c.body, logged)
			}
		})
	}
}

// The request-body gate is POST/PUT/PATCH (proxy.go:746-750), while the query
// string and the path beside it are mapped back for every method. A DELETE that
// carries a body is therefore the one write shape whose body goes upstream
// untouched — and WP_REST_Request::parse_body_params() reads a form or JSON
// body whatever the method, so those params are real.
func TestADeleteBodyIsMappedBack(t *testing.T) {
	const body = "content=%3Ca+href%3D%22https%3A%2F%2F" + variantHost + "%2Fpromo%2F%22%3Ex%3C%2Fa%3E"

	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			h := newHarness(t, acmecorpMap(t), func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(200)
			})
			req, err := http.NewRequest(method, h.front.URL+"/wp-json/wp/v2/posts/1",
				bytes.NewReader([]byte(body)))
			if err != nil {
				t.Fatal(err)
			}
			req.Host = variantHost
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			}}
			res, err := cl.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if h.seen == nil {
				t.Fatal("the upstream was never reached")
			}
			up, _ := io.ReadAll(h.seen.Body)
			if bytes.Contains(up, []byte(variantHost)) {
				t.Errorf("a variant hostname reached the shared database (PLAN §4.3)\n"+
					" sent %s\n up   %s", body, up)
			}
		})
	}
}
